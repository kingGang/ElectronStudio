package electronbot

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// libusbAPI 持有从 libusb 动态解析出的函数指针（通过 purego 绑定，无 cgo）。
type libusbAPI struct {
	handle uintptr // dlopen 句柄

	Init             func(ctx uintptr) int32
	Exit             func(ctx uintptr)
	OpenVIDPID       func(ctx uintptr, vid, pid uint16) uintptr
	Close            func(dev uintptr)
	SetAutoDetach    func(dev uintptr, enable int32) int32
	SetConfiguration func(dev uintptr, config int32) int32
	ClaimInterface   func(dev uintptr, iface int32) int32
	ReleaseInterface func(dev uintptr, iface int32) int32
	ClearHalt        func(dev uintptr, endpoint uint8) int32
	ResetDevice      func(dev uintptr) int32
	BulkTransfer     func(dev uintptr, endpoint uint8, data uintptr, length int32, transferred uintptr, timeout uint32) int32
	GetDeviceList    func(ctx uintptr, list uintptr) int64
	FreeDeviceList   func(list uintptr, unref int32)
	GetDeviceDesc    func(dev uintptr, desc uintptr) int32
	GetDevice        func(handle uintptr) uintptr // libusb_get_device：句柄→设备
	GetDeviceSpeed   func(dev uintptr) int32       // libusb_get_device_speed：连接速度(USB 2.0/3.0)
}

// candidateLibs 返回各平台 libusb 共享库的候选文件名。
func candidateLibs() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"libusb-1.0.dll"}
	case "darwin":
		return []string{"libusb-1.0.0.dylib", "libusb-1.0.dylib"}
	default: // linux / 树莓派 等
		return []string{"libusb-1.0.so.0", "libusb-1.0.so"}
	}
}

// loadLibusb 依次尝试候选名加载 libusb 并绑定所需函数。
func loadLibusb() (*libusbAPI, error) {
	var lastErr error
	for _, name := range candidateLibs() {
		h, err := dlopen(name) // 平台相关：Unix 走 purego.Dlopen，Windows 走 LoadLibrary
		if err != nil {
			lastErr = err
			continue
		}
		api := &libusbAPI{handle: h}
		purego.RegisterLibFunc(&api.Init, h, "libusb_init")
		purego.RegisterLibFunc(&api.Exit, h, "libusb_exit")
		purego.RegisterLibFunc(&api.OpenVIDPID, h, "libusb_open_device_with_vid_pid")
		purego.RegisterLibFunc(&api.Close, h, "libusb_close")
		purego.RegisterLibFunc(&api.SetAutoDetach, h, "libusb_set_auto_detach_kernel_driver")
		purego.RegisterLibFunc(&api.SetConfiguration, h, "libusb_set_configuration")
		purego.RegisterLibFunc(&api.ClaimInterface, h, "libusb_claim_interface")
		purego.RegisterLibFunc(&api.ReleaseInterface, h, "libusb_release_interface")
		purego.RegisterLibFunc(&api.ClearHalt, h, "libusb_clear_halt")
		purego.RegisterLibFunc(&api.ResetDevice, h, "libusb_reset_device")
		purego.RegisterLibFunc(&api.GetDeviceList, h, "libusb_get_device_list")
		purego.RegisterLibFunc(&api.FreeDeviceList, h, "libusb_free_device_list")
		purego.RegisterLibFunc(&api.GetDeviceDesc, h, "libusb_get_device_descriptor")
		purego.RegisterLibFunc(&api.BulkTransfer, h, "libusb_bulk_transfer")
		purego.RegisterLibFunc(&api.GetDevice, h, "libusb_get_device")
		purego.RegisterLibFunc(&api.GetDeviceSpeed, h, "libusb_get_device_speed")
		return api, nil
	}
	return nil, fmt.Errorf("electronbot: 无法加载 libusb（尝试 %v）: %w", candidateLibs(), lastErr)
}

// Probe 检查 USB 总线上是否存在受支持的 ElectronBot 设备。它只列设备 + 读描述符，
// 【绝不 open/claim】，因此不会触发设备的“连接后就绪窗口”。真正的 Connect（首次 open）
// 留给驱动循环做——只有进程的首次 open 才能可靠命中就绪窗口（实测：connectRobot 若先
// open 一次会“用掉”首次机会，之后驱动里的重连很难再命中）。
// ProbeErr 与 Probe 相同，但把"libusb 根本没装"和"设备没插/没枚举"区分开。
//
// 【为什么要分开】：以前两者都只是 return false，上层统一打一句"未探测到 ElectronBot，使用 Mock"。
// 于是 libusb 缺失（Windows 上 libusb-1.0.dll 不在工作目录/PATH）时，现象是"机器人插着、驱动
// 也正常，程序却说没探测到"，日志里一个字的线索都没有——真机排查时这一步能吃掉好几个小时。
func ProbeErr() (bool, error) {
	lib, err := loadLibusb()
	if err != nil {
		return false, err // libusb 没装：这是【环境问题】，不是"设备没插"
	}
	if lib.Init(0) != 0 {
		return false, fmt.Errorf("electronbot: libusb 初始化失败")
	}
	return probeWith(lib), nil
}

func Probe() bool {
	ok, _ := ProbeErr()
	return ok
}

// probeWith 用已加载的 libusb 扫一遍总线，看有没有受支持的设备。
func probeWith(lib *libusbAPI) bool {
	defer lib.Exit(0)

	var listPtr unsafe.Pointer
	count := lib.GetDeviceList(0, uintptr(unsafe.Pointer(&listPtr)))
	if count <= 0 {
		return false
	}
	defer lib.FreeDeviceList(uintptr(listPtr), 1)

	ptrSize := unsafe.Sizeof(uintptr(0))
	var desc [18]byte // libusb_device_descriptor：idVendor@8、idProduct@10（主机字节序）
	for i := int64(0); i < count; i++ {
		dev := *(*uintptr)(unsafe.Add(listPtr, uintptr(i)*ptrSize))
		if dev == 0 {
			break
		}
		if lib.GetDeviceDesc(dev, uintptr(unsafe.Pointer(&desc[0]))) != 0 {
			continue
		}
		vid := uint16(desc[8]) | uint16(desc[9])<<8
		pid := uint16(desc[10]) | uint16(desc[11])<<8
		for _, id := range supportedDevices {
			if id.vid == vid && id.pid == pid {
				return true
			}
		}
	}
	return false
}

// Device 是 ElectronBot 的 USB 传输实现，满足 robot.Transport。
type Device struct {
	log *slog.Logger

	// resetPort：串口软复位通道（""/"auto" 自动按 VID/PID 找 CP210x/CH340，或显式 "COM3"/"/dev/ttyUSB0"）。
	// 由 SetResetPort 在启动时设一次，Reboot 时读取。见 reboot.go。
	resetPort string

	mu     sync.Mutex
	lib    *libusbAPI
	handle uintptr // libusb_device_handle*
	// connected 用原子量而非受 mu 保护：Sync 会长时间持有 mu（bulk 读写可达数秒），
	// 若 Connected() 也走 mu 就会被 Sync 阻塞——而 statusSnapshot() 在每个新连接的
	// OnConnect 里调 Connected()，一阻塞整个网页就收不到任何消息。故用原子读，永不等锁。
	connected atomic.Bool
	// closing 由 Close 置位：让"永不放弃"的读握手在重试间隙尽快退出、释放 mu，
	// 否则设备停发就绪包时优雅退出会被卡死（读不返回→mu 不放→Close 死等）。
	closing atomic.Bool
	// everSynced：本次连接是否已经成功跑通过至少一帧。首帧要死等设备进入收发循环（连接后就绪
	// 窗口），此时 IN 读超时【不能】当作"请求包丢了"去发图，否则一上来就把它搞错位。跑通一帧后
	// 才启用 IN 读超时的 lockstep 自愈（见 bulkRetry）。
	everSynced atomic.Bool
	speed   string // USB 连接速度，如 "USB 2.0"/"USB 3.0"（连接时读取，供 UI 展示）
	// pipeRecover：累计被 clear_halt 原地恢复的 PIPE 停滞次数。macOS libusb 瞬时故障的诊断计数——
	// 节流打印，证明“跑一会儿就死”的真凶已被无损拦截(而非把设备搞掉线)。
	pipeRecover atomic.Uint64
	// retries：累计 bulk 重试次数（超时/短传/停滞都算）。失步只可能从重试里长出来，故单列计数。
	retries atomic.Uint64
	// feedbackLost：累计"反馈包被主机侧吞掉"的次数（读到成功但零长）。见 syncFrame 里的接回逻辑。
	feedbackLost atomic.Uint64
	// zlpLost：累计写失败的零长包次数（不重试、按已送达继续，见 bulkWriteZLPBestEffort）。
	zlpLost atomic.Uint64
	// writeAssumed：累计"数据包报告成功但 transferred=0"的写（同样不重发、按已送达继续，见 bulkRetry）。
	writeAssumed atomic.Uint64

	image []byte               // 整帧图像缓冲（240×240×3），跨 Sync 保留
	extra [extraDataBytes]byte  // 待下发的 extraData（使能 + 舵机设定）
	rx    [extraDataBytes]byte  // 最近一次读回的反馈
}

// New 创建一个 ElectronBot 设备（尚未连接，需调用 Connect）。
func New(log *slog.Logger) *Device {
	if log == nil {
		log = slog.Default()
	}
	return &Device{log: log, image: make([]byte, imageBytes)}
}

// Connect 实现 robot.Transport：加载 libusb、打开设备、占用接口。
func (d *Device) Connect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connected.Load() {
		return nil
	}
	d.closing.Store(false) // 重连：清掉上次 Close 的关闭标志

	lib, err := loadLibusb()
	if err != nil {
		return err
	}
	if ret := lib.Init(0); ret != 0 { // 0 = 使用默认 context
		return fmt.Errorf("electronbot: libusb_init 失败: %d", ret)
	}
	// 依次尝试已知设备标识（初代 / 精英版）。
	var handle uintptr
	var dev deviceID
	for _, id := range supportedDevices {
		if h := lib.OpenVIDPID(0, id.vid, id.pid); h != 0 {
			handle, dev = h, id
			break
		}
	}
	if handle == 0 {
		lib.Exit(0)
		return fmt.Errorf("electronbot: 未找到设备（尝试 %s，均未插入或未枚举）", deviceListString())
	}
	// Linux 上自动从内核驱动接管（其它平台返回不支持，忽略）。
	lib.SetAutoDetach(handle, 1)
	// 选择配置 #1（对照官方 .NET SDK 的 SetConfiguration(1)）：发 USB SET_CONFIGURATION 复位
	// 设备接口/端点，是触发其“连接后就绪窗口”的关键——重连循环靠它才能（最终）命中、开始同步。
	lib.SetConfiguration(handle, 1)
	if ret := lib.ClaimInterface(handle, ifaceNum); ret != 0 {
		lib.Close(handle)
		lib.Exit(0)
		return fmt.Errorf("electronbot: 占用接口失败: %d", ret)
	}
	// 清除两个 bulk 端点可能残留的 halt 状态（上次进程被强杀未干净释放时常见）。
	lib.ClearHalt(handle, endpointOut)
	lib.ClearHalt(handle, endpointIn)

	d.lib = lib
	d.handle = handle
	d.speed = speedString(lib.GetDeviceSpeed(lib.GetDevice(handle))) // USB 连接速度(2.0/3.0)，供 UI 展示
	d.everSynced.Store(false)                                        // 新连接：首帧重新按"死等就绪窗口"处理
	d.connected.Store(true)
	d.log.Info("ElectronBot 已连接", "name", dev.name, "vid", dev.vid, "pid", dev.pid, "速度", d.speed)
	return nil
}

// speedString 把 libusb_get_device_speed 的枚举映射成可读连接速度。
func speedString(speed int32) string {
	switch speed {
	case 1:
		return "USB 1.0" // LOW 1.5Mbps
	case 2:
		return "USB 1.1" // FULL 12Mbps
	case 3:
		return "USB 2.0" // HIGH 480Mbps
	case 4:
		return "USB 3.0" // SUPER 5Gbps
	case 5:
		return "USB 3.1" // SUPER_PLUS 10Gbps
	default:
		return ""
	}
}

// Speed 返回 USB 连接速度("USB 2.0"/"USB 3.0" 等)，未连接为空。不走 mu（同 Connected）：
// Sync 长时间持 mu，statusSnapshot 调 Speed() 不能被它阻塞。speed 在连接时设好、之后稳定。
func (d *Device) Speed() string {
	if !d.connected.Load() {
		return ""
	}
	return d.speed
}

// 【固件被掐死在半帧里，只能断电复位——两条"免拔电源"的自愈路都试过了，都不成立】
//
// 症状：上一个进程被【强杀】（Stop-Process -Force / 任务管理器 / 崩溃）时，Close() 的优雅退出
// 没机会跑，传输断在半帧中间。固件收帧是无超时自旋——
//
//	ReceiveUsbPacketUntilSizeIs: while (usbBuffer.receivedPacketLen != _count);  // 死等主机发完这一帧
//
// ——于是它永远停在那句 while 上等剩下的数据，不再发请求包。之后任何进程连上去都是"连上后第一次
// IN 读就超时"（首帧就超时 = 设备带着旧伤，不是本次运行搞坏的，据此可一眼区分）。
//
// 试过的两条路（2026-07-13 真机实测，都别再走）：
//
//  1. libusb_reset_device（USB 端口级复位）：复位确实执行了（返回 0），之后 IN 读照样全部超时。
//     它只重置设备的 USB 外设/端口状态，而固件卡在【应用主循环】的 while 里，那个循环压根不看
//     USB 复位事件，receivedPacketLen 也不会因此改变。
//
//  2. 盲发一整帧把它"喂饱"（4 段 × (84×512+ZLP) + 224 尾包，不读反馈）：看起来成功了——IN 读
//     恢复、零重试、Sync 不再报错。但设备其实是【僵尸】：屏幕不再刷新、6 轴反馈恒为精确的 0.00
//     （正常应带 ±0.5° 的电位器噪声）。它把一个"会明确报错、会提示用户断电"的故障，变成了
//     "看着一切正常、实际全死"的静默故障——比原来的问题更危险，故已回滚。
//
// 结论：固件真卡死就只能断电（拔线 ≥15 秒放净电容）。传输层的职责是【如实报错】，让驱动去提示
// 用户，而不是假装自己救回来了。真正该做的是【别把它掐死】——优雅退出(Ctrl+C → Close 等当前帧
// 走完)是安全的，强杀不是。

// SetImage 实现 robot.Transport：设置下一帧画面（RGB888，240×240）。
func (d *Device) SetImage(rgb888 []byte) error {
	if len(rgb888) != imageBytes {
		return fmt.Errorf("electronbot: 画面字节数不符, 期望 %d 实际 %d", imageBytes, len(rgb888))
	}
	d.mu.Lock()
	copy(d.image, rgb888)
	d.mu.Unlock()
	return nil
}

// SetJointAngles 实现 robot.Transport：设置目标舵机角度与使能。
func (d *Device) SetJointAngles(angles robot.Joints, enable bool) error {
	d.mu.Lock()
	d.extra = buildExtraData(angles, enable)
	d.mu.Unlock()
	return nil
}

// errDeviceLost 表示设备已从 USB 总线消失（libusb 返回 NO_DEVICE）。用于触发清理与重连。
var errDeviceLost = errors.New("electronbot: 设备已从总线断开")

// errFeedbackLost 表示 MCU 的 32 字节反馈包被主机 USB 栈吞了（bulk 读返回成功但零字节）。
// 不是错误、更不是掉线——设备已经发过、正在等我们发图，故 syncFrame 收到它要【继续发图】而非重读。
var errFeedbackLost = errors.New("electronbot: 反馈包丢失(读到零长)")

const errNoDevice = -4 // LIBUSB_ERROR_NO_DEVICE

// errIO：LIBUSB_ERROR_IO。句柄级 I/O 失败——设备被复位/重枚举后旧 handle 变野时，Windows/WinUSB
// 常返这个而【非】NO_DEVICE(-4)。串口软复位(见 reboot.go)后就是它。必须【当掉线处理→重连拿新 handle】：
// 它是【瞬间返回】(不像超时会阻塞)，若在旧 handle 上原地重试会满速空转烧满 CPU(实测重试累计上千万)、
// 且永远等不到自愈。真机实锤：串口复位后若不把 -1 当掉线，设备卡在 ret=-1 死循环、只有重连能救。
const errIO = -1
// errPipe：LIBUSB_ERROR_PIPE，端点管道停滞(stall)。macOS 的 libusb 在 bulk 传输上会偶发此错
// （Windows/WinUSB 不会或自动清除），是“Mac 上跑一会儿就死”的真因。正确处理是 libusb_clear_halt
// 清掉停滞后【重试】该传输，而非当掉线去 reopen（reopen 的 churn 会把固件彻底搞掉线）。
const errPipe = -9 // LIBUSB_ERROR_PIPE

// errTimeout：LIBUSB_ERROR_TIMEOUT。传输没在期限内完成——【但端点并没有 stall】。
const errTimeout = -7

// shouldClearHalt 判断某个 bulk 错误码该不该 clear_halt。
//
// 【超时不是 stall，超时后 clear_halt 会把设备搞死】。libusb_clear_halt 发的是
// CLEAR_FEATURE(ENDPOINT_HALT)，它会把主机与设备两侧的【数据翻转位(data toggle)】一并复位。
// 端点真 stall(PIPE) 时这是标准动作；但 bulk 超时时端点根本没坏，只是这次传输没按时完成
// （MCU 正忙着刷 LCD，或被同一 hub 上 USB 声卡的等时流挤了一下——本机实测：麦克风关掉后
// 3 分钟零重试，48kHz 开着 37 次、16kHz 开着 13 次，重试次数与音频码率成正比）。
//
// 此时复位 toggle，固件那台正处在 4 段 lockstep 中途的状态机就与主机对不上了：后续包要么被
// 设备当重复包丢弃、要么被重复接收，rxDataOffset 就此错位，再也等不到那个"长度=224"的尾包
// → 无超时自旋 → 主控硬死，只能断电复位。于是一次【本可恢复】的超时被我们亲手变成了永久失步。
//
// 官方 .NET SDK(maker-community/ElectronBot.DotNet 的 TransmitPacket/ReceivePacket)超时后
// 只是【原地重发同一个包】，clear_halt 一次都不调、ResetDevice 在新固件那条路里也被注释掉了。
// 那才是对的：设备迟早会把请求包发出来，重发即可接上。
//
// 唯一的例外是 macOS：IOKit 上超时的传输取消得不干净，会把整条管道堵死（一个 ZLP 超时后紧接着
// 每个 512 数据包都超时，端点整整死 12 秒），那是平台缺陷，只能靠 clear_halt 捞回来。
func shouldClearHalt(ret int32, goos string) bool {
	switch ret {
	case errPipe:
		return true // 端点真 stall：任何平台都该清
	case errTimeout:
		return goos == "darwin" // 只有 macOS 的超时会堵死管道；别处清了反而把固件搞失步
	}
	return false
}

// syncBackstop：单次 bulk 传输的“真卡死”兜底时长。远大于任何 macOS libusb 瞬时故障(PIPE停滞/单次
// 超时，至多数秒)，正常与瞬时故障下永不命中；命中即认定固件真卡死，交上层复位并提示断电。
const syncBackstop = 12 * time.Second

// errClosing：Close 置 closing 后，卡在无限重试里的传输据此尽快退出、释放 d.mu。
var errClosing = errors.New("electronbot: 传输被关闭中断")

// Sync 实现 robot.Transport：执行一次完整的图像 + 角度收发（对照官方 SyncTask）。
// 若过程中检测到设备掉线，会清理死 handle 并置为未连接，供上层退避重连。
func (d *Device) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.connected.Load() {
		return fmt.Errorf("electronbot: 未连接")
	}
	err := d.syncFrame()
	if err == nil {
		// 首帧跑通：设备已进入收发循环，之后 IN 读超时可按"请求包丢了"自愈（见 bulkRetry）。
		d.everSynced.Store(true)
	}
	if errors.Is(err, errDeviceLost) {
		d.teardownLocked() // 设备没了：释放死 handle，置未连接（Connect 可重新打开）
	}
	return err
}

// syncFrame 执行一帧的 4 段收发（持有 d.mu 时调用）。
func (d *Device) syncFrame() error {
	offset := 0
	for seg := 0; seg < segments; seg++ {
		// 1) 等待 MCU 请求并读回 32 字节反馈（官方 ReceivePacket(1,32)）。
		//    固件收发是无超时自旋，任何中途放弃都会把它卡死，故这里无限重试到读满(见 bulkRetry)。
		if err := d.bulkReadExact(endpointIn, d.rx[:]); err != nil {
			if !errors.Is(err, errFeedbackLost) {
				return err
			}
			// 反馈包在主机侧丢了（读到"成功但零长"）。此刻【设备已经把它发出去了】——固件的
			// CDC_Transmit_HS 一旦交给 USB 核心就清了 TxState、往下走，现在正卡在
			// ReceiveUsbPacketUntilSizeIs(224) 上等我们发图。
			//
			// 这时若按常规"没读满就重读"，就是死局：它不会再发，我们不会去写，两边对着干瞪眼，
			// 直到兜底超时判死（真机日志实锤：零长读之后紧跟着就是连续 5s 读超时 → 判死）。
			// 正确做法是【认定反馈已丢、直接往下发图】，把 lockstep 接回去。代价仅仅是这一帧沿用上
			// 一帧的关节反馈（UI 少更新一帧），而原本这个状态是 100% 必死、只能断电复位。
			d.feedbackLost.Add(1)
			d.log.Warn("反馈包丢失(读到零长)，按 MCU 已发送处理、继续发图以接回 lockstep",
				"累计", d.feedbackLost.Load())
		}
		// 2) 写入本段图像主体：84 个 512 字节包（每包后补一个零长包 ZLP）。
		if err := d.transmitPackets(d.image[offset:offset+segImageBytes]); err != nil {
			return err
		}
		offset += segImageBytes
		// 3) 写入尾包：192 字节图像尾 + 32 字节 extraData（224 字节，非 512 整数倍，不补 ZLP）。
		var tail [tailPacketBytes]byte
		copy(tail[:tailImageBytes], d.image[offset:offset+tailImageBytes])
		copy(tail[tailImageBytes:], d.extra[:])
		if err := d.bulkWriteExact(endpointOut, tail[:]); err != nil {
			return err
		}
		offset += tailImageBytes
	}
	return nil
}

// teardownLocked 释放当前 libusb 资源并置为未连接（调用方须持有 d.mu）。
func (d *Device) teardownLocked() {
	if d.lib != nil && d.handle != 0 {
		d.lib.ReleaseInterface(d.handle, ifaceNum)
		d.lib.Close(d.handle)
		d.lib.Exit(0)
	}
	d.lib = nil
	d.handle = 0
	d.connected.Store(false)
}

// JointAngles 实现 robot.Transport：返回最近一次 Sync 读回的真实角度。
func (d *Device) JointAngles() robot.Joints {
	d.mu.Lock()
	defer d.mu.Unlock()
	return parseFeedback(d.rx[:])
}

// Connected 实现 robot.Transport。用原子读，绝不等 Sync 的锁
//（否则 OnConnect 里的 statusSnapshot 会被长时间的 Sync 卡住，全网页失去响应）。
func (d *Device) Connected() bool {
	return d.connected.Load()
}

// Close 实现 robot.Transport：释放接口与 libusb。
func (d *Device) Close() error {
	// 【绝不半路打断正在进行的一帧】——对照官方 electron_low_level.cpp 的 Disconnect()：
	//
	//	if (syncTaskHandle.joinable()) syncTaskHandle.join();   // 先把在跑的 SyncTask 走完
	//	USB_CloseDevice(0);
	//
	// 固件的一帧是 4 段严格 lockstep（读32 → 写84×512 → 写224尾包），且收发全是【无超时死循环】。
	// 传输被从中间掐断，MCU 就永远停在 while(receivedPacketLen != 224) 上等那个再也不会来的尾包
	// → 永久自旋 → 不再发反馈 → 主控硬死（只能断电复位）。以前这里一上来就 closing=true 强行打断，
	// 于是每次 Ctrl+C / 重启程序都在亲手把固件搞死，下次连上必是"2×15s 全超时"。
	//
	// 拿到 d.mu 就等价于 join：Sync 全程持有它，锁一到手说明这帧已经走完。
	if !d.acquireGraceful(closeGrace) {
		// 等不到——设备多半本来就卡死在传输里（Sync 正堵在 backstop）。这时打断已无所谓，
		// 强行置位让它尽快放锁，否则关不掉。
		d.closing.Store(true)
		d.mu.Lock()
	}
	defer d.mu.Unlock()
	if !d.connected.Load() {
		return nil
	}
	d.lib.ReleaseInterface(d.handle, ifaceNum)
	d.lib.Close(d.handle)
	d.lib.Exit(0)
	d.connected.Store(false)
	d.log.Info("ElectronBot 已断开")
	return nil
}

// transmitPackets 逐个写出一段图像的 84 个 512 字节包，【每包后补一个零长包(ZLP)】。
//
// ZLP 不是可有可无的 Windows 仪式——它是这个固件的偏移逻辑赖以工作的一拍。看 CDC_Receive_HS：
//
//	if (len == 224) SwitchPingPongBuffer();        // 段尾：rxDataOffset=0、换缓冲
//	hcdc->RxBuffer = base + rxDataOffset;          // ← 用【旧】offset 挂下一次接收缓冲
//	if (len == 512) rxDataOffset += 512;           // ← 之后才推进 offset
//	USBD_LL_PrepareReceive(..., hcdc->RxBuffer);
//
// 赋值在推进【之前】。若只发数据包不发 ZLP：包 N 落在 base+off，回调又把缓冲挂回 base+off（因为
// 此刻 offset 还没推进），于是包 N+1 覆盖包 N——整段图错位一个包(512B≈170 像素)，屏幕横向撕裂
// （真机验证过，就是这个现象）。ZLP 进来时 len=0：不推进 offset，但会把缓冲重新挂到【已推进过】
// 的新地址，下一个真数据包才落对位置。这一拍就是它的全部作用。
//
// 但 ZLP 在 macOS 上会偶发写超时(ret=-7)，而【重试它是致命的】：真机日志——
//
//	bulk 重试 序号=1..6  ep=0x1 ret=-7 transferred=0 length=0   # 同一个 ZLP，每 2s 超一次，连超 6 次
//	bulk 重试 序号=7..9  ep=0x81 ret=-7 transferred=0/32        # 整帧卡了 12s，主控失步 → 硬死
//
// 所以：ZLP 照发，超时就【当它已经送到、绝不重试】(bulkWriteZLPBestEffort)。真丢了的代价只是这一段
// 图错一个包、屏幕闪一下，而且下一个 224 尾包会把 offset 归零自动复原；重试的代价则是必死。
func (d *Device) transmitPackets(buf []byte) error {
	for i := 0; i < packetsPerSeg; i++ {
		if err := d.bulkWriteExact(endpointOut, buf[i*packetSize:(i+1)*packetSize]); err != nil {
			return err
		}
		if err := d.bulkWriteZLPBestEffort(endpointOut); err != nil {
			return err // 只有真掉线才会走到这里
		}
	}
	return nil
}

// bulkWriteZLPBestEffort 写一个零长包：只发一次，超时/短传【一律不重试】，直接当成功往下走。
// 理由见 transmitPackets——重试 ZLP 会把整帧卡死、进而把固件搞成永久自旋（只能断电复位）；
// 而丢一个 ZLP 只会让这一段图错位一个包，下一个 224 尾包即自动复位。
func (d *Device) bulkWriteZLPBestEffort(ep uint8) error {
	var dummy [1]byte // 长度为 0，仅需一个非空指针
	var transferred int32
	ret := d.lib.BulkTransfer(d.handle, ep, uintptr(unsafe.Pointer(&dummy[0])), 0,
		uintptr(unsafe.Pointer(&transferred)), writeTimeoutMs)
	if ret == errNoDevice || ret == errIO { // 掉线/句柄失效：交上层重连（见 bulkRetry 处同款注释）
		return errDeviceLost
	}
	if ret != 0 {
		// 同 bulkRetry：超时【不】clear_halt——复位 toggle 会让固件的 lockstep 失步、硬死。
		// 仅真 stall(PIPE) 才清；macOS 的超时是平台缺陷，例外处理。见 shouldClearHalt。
		if shouldClearHalt(ret, runtime.GOOS) {
			d.lib.ClearHalt(d.handle, ep)
		}
		if n := d.zlpLost.Add(1); n%50 == 1 { // 节流打印：只在第 1、51、101… 次报
			d.log.Warn("零长包写失败，按已送达继续", "ret", ret, "累计", n)
		}
	}
	return nil
}

// bulkWriteExact / bulkReadExact 都是 bulkRetry 的薄封装(写 / 读)。
func (d *Device) bulkWriteExact(ep uint8, p []byte) error {
	return d.bulkRetry(ep, uintptr(unsafe.Pointer(&p[0])), int32(len(p)), writeTimeoutMs)
}

// bulkReadExact 读请求/反馈包。首帧用长超时死等设备进入收发循环；跑通之后改用短超时——
// 稳态下超时即判定"请求包已丢"，等得越久，同步循环白冻的时间就越长（见 readTimeoutSteadyMs）。
func (d *Device) bulkReadExact(ep uint8, p []byte) error {
	timeout := uint32(readTimeoutMs)
	if d.everSynced.Load() {
		timeout = readTimeoutSteadyMs
	}
	return d.bulkRetry(ep, uintptr(unsafe.Pointer(&p[0])), int32(len(p)), timeout)
}

// bulkRetry 执行一次 bulk 传输并按官方语义【无限原地重试】，直到读/写满 length、确实掉线或正在关闭。
//
// 这是“同样固件 Windows 不死、Mac 跑几分钟就死”的根治。固件侧(Bsp/robot.cpp)收发是【无超时自旋】：
//
//	SendUsbPacket:               do { ret = CDC_Transmit_HS(...); } while (ret != USBD_OK); // 等 host 取走
//	ReceiveUsbPacketUntilSizeIs: while (usbBuffer.receivedPacketLen != _count);            // 等 host 发齐
//
// 所以 host 只要在【帧中途】放弃(超时 give-up / reopen churn)，固件就永久卡在这两个自旋里 → 设备“死”，
// 只能断电复位。WinUSB 不丢包、不 stall，所以 Windows 永不触发；macOS libusb 会偶发 PIPE 停滞/超时，
// 旧代码 3~8s 就放弃 → 反而亲手把固件搞死。
//
// 正解(对照官方 electron_low_level.cpp 的 while(!ret) / while(ret!=size) 无限重试)：瞬时故障一律
// clear_halt 后【原地重试】、绝不中途放弃；仅真掉线(NO_DEVICE)或正在关闭(closing)才返回。backstop 仅作
// “真卡死”兜底——远大于任何瞬时故障，正常永不命中，命中即固件真卡死、交上层复位并提示断电。
func (d *Device) bulkRetry(ep uint8, ptr uintptr, length int32, timeoutMs uint32) error {
	deadline := time.Now().Add(syncBackstop)
	for {
		if d.closing.Load() { // 关闭中：尽快退出释放 d.mu
			return errClosing
		}
		var transferred int32
		ret := d.lib.BulkTransfer(d.handle, ep, ptr, length,
			uintptr(unsafe.Pointer(&transferred)), timeoutMs)
		if ret == 0 && transferred == length { // 读/写满(ZLP 时 length==0 亦成立)
			return nil
		}
		if ret == errNoDevice || ret == errIO {
			// NO_DEVICE(-4)=设备已从总线消失；IO(-1)=句柄失效(设备被复位/重枚举后旧 handle 变野，
			// 串口软复位后常见)。两者都需【重连拿新 handle】，绝不能在旧 handle 上原地重试——尤其 -1
			// 瞬间返回，重试会满速空转烧 CPU 且永不自愈。交上层 teardown→reopen 拿新 handle 接回。
			return errDeviceLost
		}
		// IN 端点上"传输成功、却一个字节都没读到"：设备确实发了包，但数据在主机 USB 栈里被吞了
		// （macOS 上摄像头/麦克风的等时流一挤就会发生）。设备那边已经认为发完、往下走了——继续重读
		// 必死锁，交给 syncFrame 去接回 lockstep。
		if ep&0x80 != 0 && ret == 0 && transferred == 0 && length > 0 {
			return errFeedbackLost
		}
		// 【同一个丢包，在 Windows 上表现为超时而非零长读】。上面那条只认 macOS 的表现形式
		// (ret=0/transferred=0)，于是 Windows 上这条自愈路径从未被触发，直接掉进死局：
		//
		//	设备：请求包已交给 USB 核心 → TxState 清零 → 往下走 → 卡在 ReceiveUsbPacketUntilSizeIs(224) 等我们发图
		//	主机：请求包被同一 hub 上 USB 声卡的等时流挤掉了 → 读不到 → 按"没收到"重读
		//	→ 它不会再发、我们不会去写，两边对着干瞪眼 → 12s 兜底判死 → 断电复位
		//
		// 真机实锤：卡死前的重试【无一例外全在 IN 端点(0x81)】，OUT 写从来没失败过——就是"设备
		// 不再发请求包"这一种死法。麦克风关掉后 3 分钟零重试；48kHz 开着 37 次、16kHz 开着 13 次，
		// 重试次数与音频码率成正比，正是等时流挤掉 IN 包的指纹。
		//
		// 所以 IN 读超时也按"请求包已丢、设备正在等图"处理，直接接回 lockstep。代价只是这一帧沿用
		// 上一帧的关节反馈；而原地重读的代价是 100% 必死。
		//
		// 【但连上后的第一帧例外】：那时设备可能还没进入收发循环（连接后就绪窗口），此刻贸然发图会
		// 让它一上来就错位。首帧仍按官方语义死等到底，只有跑通过至少一帧之后才启用这条自愈。
		if ep&0x80 != 0 && ret == errTimeout && length > 0 && d.everSynced.Load() {
			return errFeedbackLost
		}
		// 【OUT 端点上"传输成功、却一个字节都没写出去"】——与 IN 的零长读完全对称，同样【绝不能重发】。
		//
		// WinUSB 已经把这个包交给了主机控制器，设备多半已经收到，只是完成状态被回报成了 0 字节
		// （同一 hub 上 USB 声卡的等时流一挤就会出现）。此时重发，设备的 rxDataOffset 就多走一个包
		// → 它再也等不到那个"长度=224"的尾包 → 无超时自旋 → 主控硬死、只能断电复位。
		//
		// 真机实锤（14:26 那次浸泡）：
		//
		//	bulk 重试 ep=0x1 ret=0 transferred=0/512   ← 零长写，我们重发了
		//	bulk 重试 ep=0x1 ret=-7 × 6                 ← 紧接着 OUT 上所有写全部超时
		//	兜底超时 → 断开 → 固件死
		//
		// 按已送达继续，代价至多是这一段图错一个包（512B≈170 像素，屏幕闪一下），而且下一个 224
		// 尾包会把 offset 归零自动复原。这与 bulkWriteZLPBestEffort 对零长包的处置是同一个道理，
		// 只是那里只覆盖了 ZLP，漏了数据包本身。
		if ep&0x80 == 0 && ret == 0 && transferred == 0 && length > 0 {
			if n := d.writeAssumed.Add(1); n%50 == 1 { // 节流打印：第 1、51、101… 次报
				d.log.Warn("数据包零长写(报告成功但 0 字节)，按已送达继续——重发会让固件失步硬死",
					"length", length, "累计", n)
			}
			return nil
		}
		// 每一次重试都记账。【失步只可能从这里发生】：一次没送达/送了两遍的写，就会让固件的
		// rxDataOffset 错位、再也等不到那个"长度=224"的尾包 → 无超时自旋 → 主控硬死。所以要能看见
		// 卡死【之前】到底重试过什么(尤其是 OUT 端点的写)，否则只能靠猜。前若干次打全量细节。
		n := d.retries.Add(1)
		if n <= 40 {
			d.log.Warn("bulk 重试", "序号", n, "ep", fmt.Sprintf("0x%x", ep), "ret", ret,
				"transferred", transferred, "length", length)
		}
		if shouldClearHalt(ret, runtime.GOOS) {
			d.lib.ClearHalt(d.handle, ep)
			if n := d.pipeRecover.Add(1); n%200 == 0 {
				d.log.Info("管道停滞已 clear_halt 原地恢复", "累计次数", n)
			}
		}
		// 其余(超时/短读等)一律原地重试，绝不中途放弃——否则把固件卡死。
		if time.Now().After(deadline) {
			// 带上 PIPE 停滞累计次数：判断这次失步前是否发生过管道停滞(停滞→重发同一个包→固件
			// rxDataOffset 多算，是我们能想到的、主机侧唯一可能诱发失步的路径)。
			return fmt.Errorf("electronbot: bulk 兜底超时(疑似固件真卡死，需断电复位) ep=0x%x ret=%d transferred=%d/%d pipe停滞累计=%d 重试累计=%d",
				ep, ret, transferred, length, d.pipeRecover.Load(), d.retries.Load())
		}
	}
}

// closeGrace 是 Close 等待"当前这一帧走完"的宽限期。正常一帧几十毫秒，给足余量；等不到即说明
// 设备已卡死在传输里，再等无益。
const closeGrace = 3 * time.Second

// acquireGraceful 在 grace 内自旋抢 d.mu（等价于官方 Disconnect() 里的 syncTaskHandle.join()）。
// 抢到返回 true（当前帧已收工，可安全关闭）；超时返回 false（调用方需强行打断）。
func (d *Device) acquireGraceful(grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if d.mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// SetResetPort 设置串口软复位通道（""/"auto" 自动找 CP210x/CH340，或显式 "COM3"）。启动时设一次即可。
func (d *Device) SetResetPort(port string) {
	d.mu.Lock()
	d.resetPort = port
	d.mu.Unlock()
}

// Reboot 实现 robot.Rebooter：通过串口给设备发软复位指令（免拔电源）。走的是独立于 USB bulk 的
// CP210x/CH340 UART，故 bulk 卡死时它照样能发出去 → MCU 系统复位 → USB 重新枚举 → 驱动自动接回。
// 只在读 resetPort 时短暂持锁，实际串口 I/O 不持 d.mu（不阻塞设备主循环、也不碰 USB handle）。
func (d *Device) Reboot() error {
	d.mu.Lock()
	port := d.resetPort
	d.mu.Unlock()
	name, err := SendReboot(port)
	if err != nil {
		return err
	}
	d.log.Info("已发送串口软复位指令(免拔电源)", "port", name)
	return nil
}

// 编译期断言：Device 实现 robot.Transport 与 robot.Rebooter。
var (
	_ robot.Transport = (*Device)(nil)
	_ robot.Rebooter  = (*Device)(nil)
)
