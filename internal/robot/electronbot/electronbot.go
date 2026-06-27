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
	d.connected.Store(true)
	d.log.Info("ElectronBot 已连接", "name", dev.name, "vid", dev.vid, "pid", dev.pid)
	return nil
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

const errNoDevice = -4 // LIBUSB_ERROR_NO_DEVICE

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
		//    段0(帧首)可超时放弃：此时未写任何数据，放弃不致半帧失步，只是丢这帧；
		//    段1-3(帧中)永不放弃：已写过前段，中途放弃会留半帧→失步→花屏。
		if err := d.bulkReadExact(endpointIn, d.rx[:], seg == 0); err != nil {
			return err
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
	d.closing.Store(true) // 先置位：让卡在永不放弃读里的 Sync 尽快退出、放掉 d.mu
	d.mu.Lock()
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

// transmitPackets 按官方 TransmitPacket 把一段图像分成 packetsPerSeg 个 512 字节包逐个写出；
// 因 packetSize(512) 是端点最大包的整数倍，每个包后须补一个零长包(ZLP)标记传输结束。
func (d *Device) transmitPackets(buf []byte) error {
	for i := 0; i < packetsPerSeg; i++ {
		if err := d.bulkWriteExact(endpointOut, buf[i*packetSize:(i+1)*packetSize]); err != nil {
			return err
		}
		// 512 % 512 == 0：补零长包（对照官方 if (packetSize % 512 == 0) Write(...,0,...)）。
		if err := d.bulkWriteZLP(endpointOut); err != nil {
			return err
		}
	}
	return nil
}

// bulkWriteExact 写出整个 p，必要时重试（对照官方 TransmitPacket 的重试语义），带总超时兜底。
func (d *Device) bulkWriteExact(ep uint8, p []byte) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		var transferred int32
		ret := d.lib.BulkTransfer(d.handle, ep,
			uintptr(unsafe.Pointer(&p[0])), int32(len(p)),
			uintptr(unsafe.Pointer(&transferred)), writeTimeoutMs)
		if ret == 0 && int(transferred) == len(p) {
			return nil
		}
		if ret == errNoDevice {
			return errDeviceLost
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("electronbot: bulk 写超时 ep=0x%x ret=%d transferred=%d", ep, ret, transferred)
		}
	}
}

// bulkWriteZLP 写一个零长包（zero-length packet），用于在 512 整数倍包后标记传输结束。
func (d *Device) bulkWriteZLP(ep uint8) error {
	var dummy [1]byte // 长度为 0，不会被读取，仅需一个非空指针
	deadline := time.Now().Add(3 * time.Second)
	for {
		var transferred int32
		ret := d.lib.BulkTransfer(d.handle, ep,
			uintptr(unsafe.Pointer(&dummy[0])), 0,
			uintptr(unsafe.Pointer(&transferred)), writeTimeoutMs)
		if ret == 0 {
			return nil
		}
		if ret == errNoDevice {
			return errDeviceLost
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("electronbot: ZLP 写超时 ep=0x%x ret=%d", ep, ret)
		}
	}
}

// bulkReadExact 读满 p（对照官方 ReceivePacket：while 不成功就一直重试）。
//
// canGiveUp=true（一帧的【帧首读】）：设备长时间不发就绪包就 3s 放弃（尚未写图像，不致失步）。
// canGiveUp=false（【帧中读】）：给久一点（容忍舵机移动等瞬时卡顿），但兜底 8s 也放弃——否则
// 设备 brownout/卡死时 syncLoop 会永久冻住、屏幕不恢复。中途放弃会留半帧花一下，但随后 reopen
// 重连即恢复，比永久冻屏好。NO_DEVICE/closing 任何时候都立即返回。
func (d *Device) bulkReadExact(ep uint8, p []byte, canGiveUp bool) error {
	const errTimeout = -7 // LIBUSB_ERROR_TIMEOUT
	deadline := time.Now().Add(8 * time.Second)
	if canGiveUp {
		deadline = time.Now().Add(3 * time.Second)
	}
	for {
		var transferred int32
		ret := d.lib.BulkTransfer(d.handle, ep,
			uintptr(unsafe.Pointer(&p[0])), int32(len(p)),
			uintptr(unsafe.Pointer(&transferred)), readTimeoutMs)
		if ret == 0 && int(transferred) == len(p) {
			return nil
		}
		if ret == errNoDevice {
			return errDeviceLost // 掉线：退出让上层重连
		}
		if ret != 0 && ret != errTimeout {
			return fmt.Errorf("electronbot: bulk 读失败 ep=0x%x ret=%d transferred=%d", ep, ret, transferred)
		}
		// 关闭中：尽快退出释放 mu，让优雅退出/Close 不被卡死。
		if d.closing.Load() {
			return fmt.Errorf("electronbot: 读被关闭中断 ep=0x%x", ep)
		}
		// 超时过久就放弃（帧首 3s / 帧中 8s），让上层 reopen 恢复，避免永久冻屏。
		if time.Now().After(deadline) {
			if canGiveUp {
				return fmt.Errorf("electronbot: 帧首读超时 ep=0x%x（设备无就绪包）", ep)
			}
			return fmt.Errorf("electronbot: 帧中读超时 ep=0x%x（设备卡死/掉电？将重连恢复）", ep)
		}
	}
}

// 编译期断言：Device 实现 robot.Transport。
var _ robot.Transport = (*Device)(nil)
