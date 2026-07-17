// Package device 提供机器人设备的统一驱动循环。
//
// ElectronBot 的 Sync 是"图像 + 舵机角度一次发完、并读回反馈"的原子操作，因此
// 必须由【单一循环】拥有设备：动作编排只更新目标姿态，画面源只更新画面，
// 由 Driver 以固定帧率把"当前姿态 + 当前画面"一并 Sync 给设备，并把画面/反馈
// 同步广播给 UI（回调）。这避免了多处各自 Sync 造成的图像/姿态错位。
package device

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/robot"
)

// 【曾经在这里试过、失败了，别再走一遍】：固件被强杀掐死在半帧里之后，我们试过两种"免拔电源"
// 的自动解卡，真机上都不成立——
//
//	1) USB 端口级复位(libusb_reset_device)：复位执行了，之后 IN 读照样全超时。它只重置设备的
//	   USB 外设，而固件是卡在【应用主循环】的 while 自旋里，那个循环压根不看 USB 复位事件。
//	2) 盲发一整帧把它"喂饱"：看起来成功了(IN 读恢复、零重试、Sync 不再报错)，但设备其实是【僵尸】
//	   ——屏幕不刷新、6 轴反馈恒为精确的 0.00(正常带 ±0.5° 噪声)。它把一个"会明确报错、会提示用户
//	   断电"的故障，变成了"看着一切正常、实际全死"的静默故障，比原来更危险。
//
// 所以：固件真卡死就是只能断电复位（拔线 ≥15 秒放净电容）。驱动的职责是【如实报告】，见下面
// stuckTimeout 那条 Warn——不要再加"自动解卡"。真正该做的是【别把它掐死】：优雅退出(Ctrl+C 或
// /api/shutdown)会等当前帧走完，强杀(Stop-Process -Force/任务管理器/崩溃)则必然掐在半帧中间。

// Driver 以固定帧率驱动机器人传输层。
type Driver struct {
	bot    robot.Transport
	source display.Source
	log    *slog.Logger
	fps    int

	// 回调（由上层注入，用于广播给 UI）：
	onFrame  func(rgb []byte)     // 有新画面帧时（240×240 RGB888）
	onJoints func(j robot.Joints) // 周期上报舵机真实角度
	onStuck  func(stuck, recovering bool) // 设备卡死/恢复回调：stuck=持续无就绪包；recovering=正在自动软复位自救

	mu          sync.Mutex
	pose        robot.Joints
	enable      bool
	servoEnable bool         // 总开关：false 时永不下发使能（舵机不上扭矩，可手动摆姿）
	trim        robot.Joints // 机械零位补偿(度)：下发前加、读回时减，见 SetJointTrim

	// framePull：非 nil=摄像头到屏模式。syncLoop 改由它【阻塞拉一帧→同步一帧】驱动（单 owner
	// 串行 读→Sync，仿官方 EmoticonActionFrameService 单一串行发送），期间不跑自由 ticker、
	// 也无独立读帧 goroutine——消除"并发读管道帧"与 libusb 争抢卡死设备屏的路径。
	framePull atomic.Pointer[func() []byte]

	// autoReboot：检测到固件卡死时是否自动发串口软复位(免拔电源，见 robot.Rebooter)。
	// SetAutoReboot 设、syncLoop 读；关掉则只提示用户断电、不自动复位。
	autoReboot atomic.Bool

	// reenableFrames > 0 时，这么多帧内强制下发 enable=0。归零后恢复正常使能，于是设备侧看到一次
	// 0→1 跳变——固件正是靠这个跳变才会重新给舵机上扭矩：
	//
	//	if (isEnabled != (bool) ptr[0]) { isEnabled = ptr[0]; electron.SetJointEnable(isEnabled); }
	//
	// 舵机自己的过流/堵转保护一旦锁存，就会"能应答 I²C、能报位置，但电机不转"（真机实测：目标角
	// 怎么变，读回都是一个死数）。只有重新使能才解锁。见 syncLoop 里的失力检测。
	reenableFrames int
}

// Reenable 请求给舵机重新上扭矩（下发一次 enable 0→1 跳变）。舵机过载保护锁存后靠它解锁。
func (d *Driver) Reenable() {
	d.mu.Lock()
	if d.reenableFrames < reenablePulseFrames {
		d.reenableFrames = reenablePulseFrames
	}
	d.mu.Unlock()
	d.log.Info("重新使能舵机（下发 enable 0→1 跳变）")
}

// SetServoEnable 设置舵机总开关。false 时无论上层怎么 enable，下发给设备的使能位都是 0
// ——舵机不上扭矩（可手动摆姿），但角度照常下发（与官方一致，见 electronbot.buildExtraData）。
func (d *Driver) SetServoEnable(on bool) {
	d.mu.Lock()
	d.servoEnable = on
	d.mu.Unlock()
}

// SetAutoReboot 设置"卡死时自动串口软复位(免拔电源)"开关。默认由配置注入(io.auto_reboot，缺省开)。
func (d *Driver) SetAutoReboot(on bool) { d.autoReboot.Store(on) }

// Reboot 手动触发设备软复位（串口，免拔电源）。供 UI「复位电子」按钮调用（固件卡死时驱动也会自动软复位）。
// 传输不支持软复位(如 Mock)时返回错误。复位会让设备掉线 ~6s 再重连，由 syncLoop 的掉线分支自动接回。
// 会阻塞约 1s(串口写完等 MCU 收指令)，调用方宜放后台。
func (d *Driver) Reboot() error {
	rb, ok := d.bot.(robot.Rebooter)
	if !ok {
		return fmt.Errorf("device: 当前设备不支持软复位（非真机？）")
	}
	d.log.Info("手动触发串口软复位(免拔电源)")
	return rb.Reboot()
}

// 舵机失力（过流/堵转保护锁存：能应答 I²C、能报位置，但电机不转）的检测与自愈参数。
const (
	reenablePulseFrames = 3                       // 重新使能脉冲：连续几帧下发 enable=0，制造 0→1 跳变
	limpSettleTime      = 1200 * time.Millisecond // 目标角保持不变多久后才开始判定（等舵机走到位）
	limpTolerance       = 15                      // 读回与下发相差多少度算"没跟上"
	limpConfirmTime     = 2 * time.Second         // 持续这么久仍不跟随，才判定失力（避开瞬时噪声）
	limpCooldown        = 20 * time.Second        // 两次自动重新使能之间的最短间隔，防止反复抽搐
	limpMaxAttempts     = 3                       // 连续这么多次救不回来就放弃（避免无休止 toggle）
)

// limpWatch 盯着"下发角度 vs 读回角度"：目标静止一段时间后仍有轴长期对不上，就判定该轴失力，
// 自动下发一次 enable 0→1 跳变把它解锁（固件收到跳变才会重新给舵机上扭矩）。
//
// 【只在目标静止时判定】：运动中舵机本来就落后于关键帧，那不是失力。
// 【姿态一变就清账】：避免把一次正常的大幅运动误判成失力。
type limpWatch struct {
	lastPose   robot.Joints
	stableFrom time.Time // 目标角保持不变的起始时刻
	badFrom    time.Time // 开始持续不跟随的时刻（零值=当前跟得上）
	lastFix    time.Time // 上次自动重新使能的时刻
	attempts   int       // 连续救援次数
}

func (w *limpWatch) check(d *Driver, pose, fb robot.Joints, enabled bool) {
	now := time.Now()
	if !enabled { // 没上扭矩，本来就不该跟随
		w.stableFrom, w.badFrom = time.Time{}, time.Time{}
		w.lastPose = pose
		return
	}
	if pose != w.lastPose { // 目标变了：重新计时，之前的账一笔勾销
		w.lastPose, w.stableFrom, w.badFrom = pose, now, time.Time{}
		return
	}
	if w.stableFrom.IsZero() || now.Sub(w.stableFrom) < limpSettleTime {
		return // 还没静止够久，舵机可能正在走过去
	}
	worst, worstJoint := float32(0), -1
	for i := range pose {
		if dev := abs32(fb[i] - pose[i]); dev > limpTolerance && dev > worst {
			worst, worstJoint = dev, i
		}
	}
	if worstJoint < 0 { // 都跟得上：清账，救援计数归零
		w.badFrom, w.attempts = time.Time{}, 0
		return
	}
	if w.badFrom.IsZero() {
		w.badFrom = now
		return
	}
	if now.Sub(w.badFrom) < limpConfirmTime || now.Sub(w.lastFix) < limpCooldown {
		return
	}
	if w.attempts >= limpMaxAttempts {
		return // 救不回来了，别再抽搐；日志已提示过
	}
	w.attempts++
	w.lastFix, w.badFrom = now, time.Time{}
	d.log.Warn("舵机疑似失力（能报位置但不跟随），自动重新使能",
		"关节", robot.JointNames[worstJoint], "下发", pose[worstJoint], "读回", fb[worstJoint],
		"第几次", w.attempts)
	d.Reenable()
	if w.attempts >= limpMaxAttempts {
		d.log.Warn("舵机自动重新使能多次仍不跟随，停止重试——请检查该舵机是否堵转/机械卡死",
			"关节", robot.JointNames[worstJoint])
	}
}

func abs32(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}

// 卡死自动软复位（串口，免拔电源）的参数。判定卡死后按冷却+次数上限发串口复位；救不回就停手等断电。
const (
	rebootCooldown    = 25 * time.Second // 两次自动软复位之间的最短间隔（复位+重枚举约 6s，留足观察窗口）
	rebootMaxAttempts = 2                // 连续这么多次软复位仍卡死就放弃、提示断电（避免复位死循环）
)

// rebootWatch 跟踪自动软复位的次数与冷却。设备判定卡死时，在同一 handle 继续硬顶的同时，按冷却+上限
// 发串口软复位去救（对照官方 ElectronBot.DotNet 的 RebootElectron）；救回来(重连并跑通一帧)即清零。
type rebootWatch struct {
	attempts int
	lastTry  time.Time
}

// try 在设备卡死时按冷却+上限发一次串口软复位。传输不支持软复位(Mock)或开关关闭时静默跳过。
func (w *rebootWatch) try(d *Driver) {
	if !d.autoReboot.Load() {
		return
	}
	rb, ok := d.bot.(robot.Rebooter)
	if !ok {
		return // Mock 或不支持软复位的传输：只能等用户断电
	}
	if w.attempts >= rebootMaxAttempts {
		return // 复位多次仍卡死：放弃，日志已提示断电
	}
	if !w.lastTry.IsZero() && time.Since(w.lastTry) < rebootCooldown {
		return // 冷却中：给上一次复位留足重枚举+观察的时间
	}
	w.lastTry = time.Now()
	w.attempts++
	d.log.Warn("卡死自动软复位(串口，免拔电源)", "第几次", w.attempts, "上限", rebootMaxAttempts)
	if err := rb.Reboot(); err != nil {
		d.log.Warn("串口软复位失败(回退：请手动断电复位)", "err", err)
	}
	if w.attempts >= rebootMaxAttempts {
		d.log.Warn("已自动软复位多次仍卡死——请彻底断电(拔线≥15秒放净电容)再插")
	}
}

// recovering 报告自动软复位是否"正在自救中"（供 UI 区分"自动复位中"与"真需断电"）。
// 开关关/传输不支持 → 否；还有复位次数没用完 → 是；用完了但上一次复位还在观察窗(冷却)内 → 仍是；
// 超了还卡着才算彻底失败(否)。
func (w *rebootWatch) recovering(d *Driver) bool {
	if !d.autoReboot.Load() {
		return false
	}
	if _, ok := d.bot.(robot.Rebooter); !ok {
		return false
	}
	if w.attempts < rebootMaxAttempts {
		return true
	}
	return !w.lastTry.IsZero() && time.Since(w.lastTry) < rebootCooldown
}

// SetJointTrim 设置 6 轴机械零位补偿(度)。装配公差让"角度 0"不一定是端正姿态（例如头部归零时略微
// 低头），这里把补偿吸收在驱动层：下发给设备前加上 trim，读回真实角度时减掉 trim。于是上层（滑杆、
// 动作编排、内置动作）看到的永远是"0 = 端正"的理想坐标系，不必每个动作各自去凑偏移。
func (d *Driver) SetJointTrim(t robot.Joints) {
	d.mu.Lock()
	d.trim = t
	d.mu.Unlock()
}

// applyTrim 把上层角度换成设备角度（加补偿并裁剪到量程内）。
func applyTrim(pose, trim robot.Joints) robot.Joints {
	for i := range pose {
		pose[i] = robot.ClampAngle(i, pose[i]+trim[i])
	}
	return pose
}

// stripTrim 把设备读回的角度换回上层角度（减掉补偿）。
func stripTrim(fb, trim robot.Joints) robot.Joints {
	for i := range fb {
		fb[i] -= trim[i]
	}
	return fb
}

// SetStuckHandler 注入“设备卡死/恢复”回调（上层据此广播 UI）。stuck=持续无就绪包；
// recovering=正在自动串口软复位自救(免拔电源)——前端据此显示"自动复位中"而非"请断电"。
func (d *Driver) SetStuckHandler(f func(stuck, recovering bool)) { d.onStuck = f }

// SetFramePuller 设置/清除摄像头到屏的"拉帧"函数。非 nil 时 syncLoop 进入摄像头模式：由它
// 阻塞拉取下一帧、拉到即 Sync(来一帧发一帧、同一帧也广播给 UI)，单 owner 串行；nil 时回落到
// 表情脸 ticker 模式。开摄像头：先 Start(ScreenMode) 再 SetFramePuller(cam.ReadFrame)；
// 关摄像头：先 SetFramePuller(nil) 再 cam.Stop()(关管道解阻塞在途的拉帧)。
func (d *Driver) SetFramePuller(pull func() []byte) {
	if pull == nil {
		d.framePull.Store(nil)
		return
	}
	d.framePull.Store(&pull)
}

func (d *Driver) notifyStuck(stuck, recovering bool) {
	if d.onStuck != nil {
		d.onStuck(stuck, recovering)
	}
}

// NewDriver 创建设备驱动。source 可为 nil（不推画面）。fps<=0 时取 30。
func NewDriver(bot robot.Transport, source display.Source, log *slog.Logger, fps int,
	onFrame func([]byte), onJoints func(robot.Joints)) *Driver {
	if log == nil {
		log = slog.Default()
	}
	if fps <= 0 {
		fps = 30
	}
	return &Driver{bot: bot, source: source, log: log, fps: fps, onFrame: onFrame, onJoints: onJoints}
}

// SetPose 设置目标舵机角度与使能。实现 choreography.PoseSink，供动作编排/手动/跟随写入。
func (d *Driver) SetPose(angles robot.Joints, enable bool) {
	d.mu.Lock()
	d.pose = angles
	d.enable = enable
	d.mu.Unlock()
}

// Run 启动驱动循环，阻塞至 ctx 取消。
// Run 跑两个解耦的循环并阻塞至 ctx 取消：
//   - frameLoop：固定帧率取画面、广播给 UI（镜像/预览）。【不碰设备】，所以设备 Sync
//     再慢/失联都不拖垮网页帧率——这是表情预览能实时的前提。
//   - syncLoop：把最近画面 + 姿态原子地 Sync 给设备；设备掉线则退避重连。设备读握手
//     时快时慢，允许它在此循环里慢慢阻塞，不影响 UI。
//
// 设备仍是【单一拥有者】（只有 syncLoop 碰设备），不破坏图像/姿态原子性。
func (d *Driver) Run(ctx context.Context) {
	var fmu sync.Mutex
	var latest []byte // 最近捕获的画面（frameLoop 写、syncLoop 读，copy 进出避免撕裂）

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); d.frameLoop(ctx, &fmu, &latest) }()
	go func() { defer wg.Done(); d.syncLoop(ctx, &fmu, &latest) }()
	wg.Wait()
}

// frameLoop 固定帧率取画面并广播给 UI；画面未变则每 ~200ms 重发上一帧。不触碰设备。
func (d *Driver) frameLoop(ctx context.Context, fmu *sync.Mutex, latest *[]byte) {
	if d.source == nil || d.onFrame == nil {
		return
	}
	ticker := time.NewTicker(time.Second / time.Duration(d.fps))
	defer ticker.Stop()
	var lastBroadcast time.Time
	const keepaliveEvery = 200 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if d.framePull.Load() != nil {
				continue // 摄像头模式：syncLoop 直接拉帧并广播同一帧，frameLoop 让位，避免双广播
			}
			if frame := d.source.Frame(); frame != nil {
				fmu.Lock()
				if len(*latest) != len(frame) {
					*latest = make([]byte, len(frame))
				}
				copy(*latest, frame)
				fmu.Unlock()
				d.onFrame(frame)
				lastBroadcast = time.Now()
			} else if time.Since(lastBroadcast) >= keepaliveEvery {
				fmu.Lock()
				f := *latest
				fmu.Unlock()
				if f != nil {
					d.onFrame(f)
					lastBroadcast = time.Now()
				}
			}
		}
	}
}

// syncLoop 把最近画面 + 姿态 Sync 给设备（设备唯一拥有者）。
//
// 设备有个“连接后就绪窗口”：host 必须连上后【立刻】开始 Sync，否则它就不再发就绪包
// （帧首读会一直超时）。所以这里先 reopen（Close+Connect 刷新窗口）再【紧接着】Sync——
// sync-then-wait 结构让首帧紧贴重连、命中窗口。命中后持续同步、不再 reopen。
// 没命中则【退避】reopen（间隔逐次翻倍封顶）——频繁重连是把固件搞卡死的元凶，越没命中越少碰；
// 连续多次仍无就绪包即判定卡死、回调上层提示用户彻底断电复位（churn 救不回）。
func (d *Driver) syncLoop(ctx context.Context, fmu *sync.Mutex, latest *[]byte) {
	ticker := time.NewTicker(time.Second / time.Duration(d.fps))
	defer ticker.Stop()
	tick := 0
	var buf []byte // syncLoop 私有缓冲，避免与 frameLoop 共享底层数组
	var lastReopen time.Time
	// 重连策略（对照 ElectronBot.DotNet：连一次、错就跳帧、绝不 churn）：只有【真掉线(NO_DEVICE)】或
	// 【连上却一帧都没跑通(疑似错过就绪窗口)】才 Close+Connect，且退避间隔逐次翻倍封顶。一旦跑通过一帧、
	// 之后只是传输卡顿，就【绝不重连】、在同一 handle 上继续硬顶——频繁 Close+Connect（churn，会重发
	// SET_CONFIGURATION 撞在固件半帧等待上）正是把它搞进“停发就绪包”深度卡死的元凶。连续无就绪包超过
	// stuckTimeout 仍在枚举，就判定卡死、回调上层提示用户断电复位（churn 救不回，只有彻底断电放电才行）。
	var limp limpWatch     // 舵机失力检测（能报位置但不转）→ 自动重新使能
	var reboot rebootWatch // 卡死自动软复位（串口，免拔电源）→ 冷却+上限内自动救
	backoff := 2 * time.Second
	const backoffMax = 10 * time.Second
	// stuckTimeout：连续无就绪包多久后判定固件真卡死、提示断电复位。远大于任何瞬时拥塞（单帧至多命中
	// 12s backstop 即恢复），故正常/瞬时拥塞下永不误判；只有“连着设备却一帧都跑不通”才会累计到它。
	const stuckTimeout = 20 * time.Second
	var failingSince time.Time // 本轮连续失败的起点（零值=当前不在失败中）；跑通一帧即清零
	stuck := false
	// lastStuck/lastRecovering：上次广播给 UI 的健康态，仅在变化时再广播（避免每帧刷 status）。
	lastStuck, lastRecovering := false, false
	// syncedSinceConnect：本次连接是否已跑通过至少一帧。仿 ElectronBot.DotNet——一旦跑通，之后的
	// 传输卡顿【绝不 Close+Connect 重连】，只在同一 handle 上继续硬顶（见下方失败分支）。
	syncedSinceConnect := false
	reopen := func() {
		lastReopen = time.Now()
		_ = d.bot.Close()
		syncedSinceConnect = false // 新连接：跑通一帧前不算“已就绪”
		if err := d.bot.Connect(); err != nil {
			d.log.Debug("重开设备失败", "err", err)
			return
		}
		// Connect 后【零间隔】立刻 Sync 一次抢就绪窗口——这一步只碰设备自身的锁(d.mu)，
		// 不取 fmu/pose 锁、不与 frameLoop 抢，尽量复刻 ebtest 的“Connect 后即刻 Sync”，
		// 把 Connect→首读 的抖动压到最小（窗口很短，差几毫秒就错过）。
		if err := d.bot.Sync(); err == nil {
			syncedSinceConnect = true
		}
	}
	reopen()

	for {
		synced := false
		camMode := d.framePull.Load()
		blockingPaced := false // 本轮是否已被"阻塞拉帧"节流（是则末尾不再等 ticker，避免帧率减半）
		if d.bot.Connected() {
			if camMode != nil {
				// 摄像头模式：阻塞拉一帧(来一帧发一帧，单 owner 串行)。拉到的同一帧既上屏也广播给 UI。
				// 拉帧本身即节流(随采集帧率)，故本轮末尾不再等 ticker。Stop 关管道 → 这里返回 nil。
				blockingPaced = true
				if frame := (*camMode)(); frame != nil {
					if len(buf) != len(frame) {
						buf = make([]byte, len(frame))
					}
					copy(buf, frame)
					if d.onFrame != nil {
						d.onFrame(frame)
					}
				}
			} else {
				// 取最近画面（copy 出来防与 frameLoop 写入撞车）推给设备。
				fmu.Lock()
				if n := len(*latest); n > 0 {
					if len(buf) != n {
						buf = make([]byte, n)
					}
					copy(buf, *latest)
				}
				fmu.Unlock()
			}
			if len(buf) > 0 {
				if err := d.bot.SetImage(buf); err != nil {
					d.log.Debug("设置画面失败", "err", err)
				}
			}
			d.mu.Lock()
			pose, enable, servoOK, trim := d.pose, d.enable, d.servoEnable, d.trim
			if d.reenableFrames > 0 { // 重新使能脉冲：这几帧强行发 enable=0，之后的 0→1 跳变解锁舵机
				d.reenableFrames--
				enable = false
			}
			d.mu.Unlock()
			_ = d.bot.SetJointAngles(applyTrim(pose, trim), enable && servoOK) // 总开关关时永不使能
			if err := d.bot.Sync(); err != nil {
				d.log.Debug("同步失败", "err", err)
			} else {
				synced = true
				tick++
				fb := stripTrim(d.bot.JointAngles(), trim)
				limp.check(d, pose, fb, enable && servoOK) // 失力检测 → 必要时自动重新使能
				if tick%3 == 0 && d.onJoints != nil {
					d.onJoints(fb)
				}
			}
		}
		if synced {
			// 命中：清零失败计时与卡死状态（之后掉线还能快速重试）。UI 广播在下方按变化统一触发。
			syncedSinceConnect = true
			failingSince = time.Time{}
			backoff = 2 * time.Second
			reboot = rebootWatch{} // 恢复后重置软复位预算（下次卡死可再自动救）
			if stuck {
				stuck = false
				d.log.Info("设备同步已恢复")
			}
		} else {
			// 本帧未同步。核心原则（对照 ElectronBot.DotNet 的低层与上层：连一次、错就跳帧、绝不 churn）：
			//   · 已跑通过至少一帧、现在只是传输卡顿 → 【绝不 reopen】，在同一 handle 上继续硬顶下一帧，
			//     等拥塞过去自然恢复。Close+Connect 会重发 SET_CONFIGURATION，正撞在固件半帧等待上把它
			//     彻底搞死——churn 才是死总线的元凶（我们自己的注释与官方源码都证实这一点）。
			//   · 真掉线(NO_DEVICE，Connected()=false，含用户断电复位后重插) → 必须重连才能找回设备，
			//     此时设备已不在，reopen 谈不上“打断半帧”。
			//   · 连上了却一帧都没跑通(疑似错过就绪窗口) → 有限退避 reopen 去重抢窗口，判死后停手。
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			disconnected := !d.bot.Connected()
			if disconnected && stuck {
				// 卡死后设备被拔走（用户断电复位 / 串口软复位重枚举中）：转入重连去接回，不再是"卡死等断电"。
				stuck = false
			}
			// 连续失败够久且设备仍在枚举 → 判卡死（真掉线不算卡死，靠重连恢复）。先尝试自动软复位，别急着喊断电。
			if !disconnected && !stuck && time.Since(failingSince) >= stuckTimeout {
				stuck = true
				d.log.Warn("设备持续无就绪包，疑似固件卡死——先尝试自动串口软复位(免拔电源)，多次无效再断电")
			}
			// 判定卡死后：自动串口软复位（免拔电源）去救——冷却+上限内发一次，复位后设备会掉线重枚举、
			// 由上面的掉线分支自动接回。救不回(超上限)就停手、等用户断电。仅设备仍在枚举时才发。
			if stuck && !disconnected {
				reboot.try(d)
			}
			// 仅两种情况才动连接：真掉线（总要重连找回设备）；或连上但从未跑通一帧且尚未判死（有限退避
			// 抢就绪窗口）。【已跑通过一帧的卡顿】两个条件都不满足 → 什么都不做，落到末尾 ticker 等下一帧
			// 重试同一 handle，这正是“不 churn”的关键。
			if (disconnected || (!syncedSinceConnect && !stuck)) && time.Since(lastReopen) >= backoff {
				reopen()
				backoff *= 2
				if backoff > backoffMax {
					backoff = backoffMax
				}
			}
		}
		// 统一广播设备健康态（仅在变化时，避免每帧刷 status）：recovering=正在自动软复位自救(免拔电源)；
		// stuck && !recovering = 自动复位无效、需手动断电。前端据此显示"自动复位中"或"请断电"。
		nowRecovering := stuck && reboot.recovering(d)
		if stuck != lastStuck || nowRecovering != lastRecovering {
			lastStuck, lastRecovering = stuck, nowRecovering
			d.notifyStuck(stuck, nowRecovering)
		}
		if blockingPaced {
			// 摄像头模式且已阻塞拉帧：拉取本身即节流，不再等 ticker，只检查退出。
			select {
			case <-ctx.Done():
				return
			default:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
