// Package device 提供机器人设备的统一驱动循环。
//
// ElectronBot 的 Sync 是"图像 + 舵机角度一次发完、并读回反馈"的原子操作，因此
// 必须由【单一循环】拥有设备：动作编排只更新目标姿态，画面源只更新画面，
// 由 Driver 以固定帧率把"当前姿态 + 当前画面"一并 Sync 给设备，并把画面/反馈
// 同步广播给 UI（回调）。这避免了多处各自 Sync 造成的图像/姿态错位。
package device

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/robot"
)

// Driver 以固定帧率驱动机器人传输层。
type Driver struct {
	bot    robot.Transport
	source display.Source
	log    *slog.Logger
	fps    int

	// 回调（由上层注入，用于广播给 UI）：
	onFrame  func(rgb []byte)     // 有新画面帧时（240×240 RGB888）
	onJoints func(j robot.Joints) // 周期上报舵机真实角度
	onStuck  func(stuck bool)     // 设备疑似卡死/恢复时回调（true=持续无就绪包，需断电复位）

	mu          sync.Mutex
	pose        robot.Joints
	enable      bool
	servoEnable bool         // 总开关：false 时永不下发使能（舵机不上扭矩，可手动摆姿）
	trim        robot.Joints // 机械零位补偿(度)：下发前加、读回时减，见 SetJointTrim

	// framePull：非 nil=摄像头到屏模式。syncLoop 改由它【阻塞拉一帧→同步一帧】驱动（单 owner
	// 串行 读→Sync，仿官方 EmoticonActionFrameService 单一串行发送），期间不跑自由 ticker、
	// 也无独立读帧 goroutine——消除"并发读管道帧"与 libusb 争抢卡死设备屏的路径。
	framePull atomic.Pointer[func() []byte]
}

// SetServoEnable 设置舵机总开关。false 时无论上层怎么 enable，下发给设备的使能位都是 0
// ——舵机不上扭矩（可手动摆姿），但角度照常下发（与官方一致，见 electronbot.buildExtraData）。
func (d *Driver) SetServoEnable(on bool) {
	d.mu.Lock()
	d.servoEnable = on
	d.mu.Unlock()
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

// SetStuckHandler 注入“设备卡死/恢复”回调（上层据此广播 UI 提示断电复位）。
func (d *Driver) SetStuckHandler(f func(stuck bool)) { d.onStuck = f }

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

func (d *Driver) notifyStuck(stuck bool) {
	if d.onStuck != nil {
		d.onStuck(stuck)
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
	// 退避重连：没命中就重连，但间隔逐次翻倍封顶——频繁 Close+Connect（churn）正是把固件
	// 搞进“停发就绪包”深度卡死的元凶，越没命中越要少碰它。连续 stuckAfter 次仍无就绪包，
	// 就判定卡死、回调上层提示用户断电复位（churn 救不回，只有彻底断电放电才行）。
	backoff := 2 * time.Second
	const backoffMax = 10 * time.Second
	const stuckAfter = 5
	failStreak := 0
	stuck := false
	reopen := func() {
		lastReopen = time.Now()
		_ = d.bot.Close()
		if err := d.bot.Connect(); err != nil {
			d.log.Debug("重开设备失败", "err", err)
			return
		}
		// Connect 后【零间隔】立刻 Sync 一次抢就绪窗口——这一步只碰设备自身的锁(d.mu)，
		// 不取 fmu/pose 锁、不与 frameLoop 抢，尽量复刻 ebtest 的“Connect 后即刻 Sync”，
		// 把 Connect→首读 的抖动压到最小（窗口很短，差几毫秒就错过）。
		_ = d.bot.Sync()
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
			d.mu.Unlock()
			_ = d.bot.SetJointAngles(applyTrim(pose, trim), enable && servoOK) // 总开关关时永不使能
			if err := d.bot.Sync(); err != nil {
				d.log.Debug("同步失败", "err", err)
			} else {
				synced = true
				tick++
				if tick%3 == 0 && d.onJoints != nil {
					d.onJoints(stripTrim(d.bot.JointAngles(), trim))
				}
			}
		}
		if synced {
			// 命中：清零退避与卡死状态（之后掉线还能快速重试），并通知 UI 恢复。
			if failStreak > 0 {
				failStreak = 0
				backoff = 2 * time.Second
			}
			if stuck {
				stuck = false
				d.log.Info("设备同步已恢复")
				d.notifyStuck(false)
			}
		} else if time.Since(lastReopen) >= backoff {
			// 未同步（帧首超时=窗口过期/掉线）：退避 reopen 并立刻重试 Sync（reopen 内已含
			// Connect 后零间隔 Sync，紧贴 Connect 命中就绪窗口）。间隔逐次翻倍，减少 churn。
			failStreak++
			reopen()
			if failStreak >= stuckAfter && !stuck {
				stuck = true
				d.log.Warn("设备持续无就绪包，疑似固件卡死——请彻底断电(拔线≥15秒放净电容)再插；频繁重连不会自愈、只会加重")
				d.notifyStuck(true)
			}
			backoff *= 2
			maxBackoff := backoffMax
			if stuck {
				maxBackoff = 30 * time.Second // 已判卡死：几乎停手等用户断电(churn 救不回、只会加重)
			}
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
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
