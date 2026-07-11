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
func Probe() bool {
	lib, err := loadLibusb()
	if err != nil || lib.Init(0) != 0 {
		return false
	}
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

// Reset 对设备做一次 USB 端口级复位（软重启其 USB 栈），用于解开固件卡死/失步态而
// 无需物理拔插。复位后设备通常会重新枚举（NOT_FOUND），此时自动重连。
func (d *Device) Reset() error {
	d.mu.Lock()
	lib, h := d.lib, d.handle
	d.mu.Unlock()
	if lib == nil || h == 0 {
		return fmt.Errorf("electronbot: 未连接，无法复位")
	}
	if ret := lib.ResetDevice(h); ret == 0 {
		d.log.Info("设备已软复位（handle 仍有效）")
		return nil
	}
	// 复位导致重新枚举：清理旧 handle 后重连。
	d.log.Info("设备软复位后重新枚举，重连中…")
	_ = d.Close()
	return d.Connect()
}

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
// errPipe：LIBUSB_ERROR_PIPE，端点管道停滞(stall)。macOS 的 libusb 在 bulk 传输上会偶发此错
// （Windows/WinUSB 不会或自动清除），是“Mac 上跑一会儿就死”的真因。正确处理是 libusb_clear_halt
// 清掉停滞后【重试】该传输，而非当掉线去 reopen（reopen 的 churn 会把固件彻底搞掉线）。
const errPipe = -9 // LIBUSB_ERROR_PIPE

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
	if ret == errNoDevice {
		return errDeviceLost
	}
	if ret != 0 {
		if n := d.zlpLost.Add(1); n%50 == 1 { // 节流打印：只在第 1、51、101… 次报
			d.log.Warn("零长包写失败，按已送达继续(重试它会把固件卡死)", "ret", ret, "累计", n)
		}
	}
	return nil
}

// bulkWriteExact / bulkReadExact 都是 bulkRetry 的薄封装(写 / 读)。
func (d *Device) bulkWriteExact(ep uint8, p []byte) error {
	return d.bulkRetry(ep, uintptr(unsafe.Pointer(&p[0])), int32(len(p)), writeTimeoutMs)
}

func (d *Device) bulkReadExact(ep uint8, p []byte) error {
	return d.bulkRetry(ep, uintptr(unsafe.Pointer(&p[0])), int32(len(p)), readTimeoutMs)
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
		if ret == errNoDevice {
			return errDeviceLost // 真掉线：退出让上层重连
		}
		// IN 端点上"传输成功、却一个字节都没读到"：设备确实发了包，但数据在主机 USB 栈里被吞了
		// （macOS 上摄像头/麦克风的等时流一挤就会发生）。设备那边已经认为发完、往下走了——继续重读
		// 必死锁，交给 syncFrame 去接回 lockstep。
		if ep&0x80 != 0 && ret == 0 && transferred == 0 && length > 0 {
			return errFeedbackLost
		}
		// 每一次重试都记账。【失步只可能从这里发生】：一次没送达/送了两遍的写，就会让固件的
		// rxDataOffset 错位、再也等不到那个"长度=224"的尾包 → 无超时自旋 → 主控硬死。所以要能看见
		// 卡死【之前】到底重试过什么(尤其是 OUT 端点的写)，否则只能靠猜。前若干次打全量细节。
		n := d.retries.Add(1)
		if n <= 40 {
			d.log.Warn("bulk 重试", "序号", n, "ep", fmt.Sprintf("0x%x", ep), "ret", ret,
				"transferred", transferred, "length", length)
		}
		if ret == errPipe {
			d.lib.ClearHalt(d.handle, ep) // 清管道停滞后原地重试(macOS libusb 已知瞬时故障，非掉线)
			if n := d.pipeRecover.Add(1); n%200 == 0 {
				d.log.Info("PIPE 停滞已 clear_halt 原地恢复(若是旧代码这些早把设备搞死了)", "累计次数", n)
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

// 编译期断言：Device 实现 robot.Transport。
var _ robot.Transport = (*Device)(nil)
