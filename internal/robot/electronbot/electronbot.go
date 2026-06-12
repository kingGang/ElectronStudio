package electronbot

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
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
	ClaimInterface   func(dev uintptr, iface int32) int32
	ReleaseInterface func(dev uintptr, iface int32) int32
	BulkTransfer     func(dev uintptr, endpoint uint8, data uintptr, length int32, transferred uintptr, timeout uint32) int32
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
		purego.RegisterLibFunc(&api.ClaimInterface, h, "libusb_claim_interface")
		purego.RegisterLibFunc(&api.ReleaseInterface, h, "libusb_release_interface")
		purego.RegisterLibFunc(&api.BulkTransfer, h, "libusb_bulk_transfer")
		return api, nil
	}
	return nil, fmt.Errorf("electronbot: 无法加载 libusb（尝试 %v）: %w", candidateLibs(), lastErr)
}

// Device 是 ElectronBot 的 USB 传输实现，满足 robot.Transport。
type Device struct {
	log *slog.Logger

	mu        sync.Mutex
	lib       *libusbAPI
	handle    uintptr // libusb_device_handle*
	connected bool

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
	if d.connected {
		return nil
	}

	lib, err := loadLibusb()
	if err != nil {
		return err
	}
	if ret := lib.Init(0); ret != 0 { // 0 = 使用默认 context
		return fmt.Errorf("electronbot: libusb_init 失败: %d", ret)
	}
	handle := lib.OpenVIDPID(0, vid, pid)
	if handle == 0 {
		lib.Exit(0)
		return fmt.Errorf("electronbot: 未找到设备 %04x:%04x（未插入或驱动未装）", vid, pid)
	}
	// Linux 上自动从内核驱动接管（其它平台返回不支持，忽略）。
	lib.SetAutoDetach(handle, 1)
	if ret := lib.ClaimInterface(handle, ifaceNum); ret != 0 {
		lib.Close(handle)
		lib.Exit(0)
		return fmt.Errorf("electronbot: 占用接口失败: %d", ret)
	}

	d.lib = lib
	d.handle = handle
	d.connected = true
	d.log.Info("ElectronBot 已连接", "vid", vid, "pid", pid)
	return nil
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

// Sync 实现 robot.Transport：执行一次完整的图像 + 角度收发（对照官方 SyncTask）。
func (d *Device) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.connected {
		return fmt.Errorf("electronbot: 未连接")
	}

	offset := 0
	for seg := 0; seg < segments; seg++ {
		// 1) 等待 MCU 请求并读回 32 字节反馈。
		if err := d.bulkReadExact(endpointIn, d.rx[:]); err != nil {
			return err
		}
		// 2) 写入本段图像主体：84 个 512 字节包。
		if err := d.transmitPackets(d.image[offset:offset+segImageBytes]); err != nil {
			return err
		}
		offset += segImageBytes
		// 3) 写入尾包：192 字节图像尾 + 32 字节 extraData。
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

// JointAngles 实现 robot.Transport：返回最近一次 Sync 读回的真实角度。
func (d *Device) JointAngles() robot.Joints {
	d.mu.Lock()
	defer d.mu.Unlock()
	return parseFeedback(d.rx[:])
}

// Connected 实现 robot.Transport。
func (d *Device) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// Close 实现 robot.Transport：释放接口与 libusb。
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.connected {
		return nil
	}
	d.lib.ReleaseInterface(d.handle, ifaceNum)
	d.lib.Close(d.handle)
	d.lib.Exit(0)
	d.connected = false
	d.log.Info("ElectronBot 已断开")
	return nil
}

// transmitPackets 按官方方式把一段图像分成 packetsPerSeg 个 packetSize 包逐个写出。
func (d *Device) transmitPackets(buf []byte) error {
	for i := 0; i < packetsPerSeg; i++ {
		if err := d.bulkWriteExact(endpointOut, buf[i*packetSize:(i+1)*packetSize]); err != nil {
			return err
		}
	}
	return nil
}

// bulkWriteExact 写出整个 p，必要时重试（对照官方 TransmitPacket 的重试语义），带总超时兜底。
func (d *Device) bulkWriteExact(ep uint8, p []byte) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		var transferred int32
		ret := d.lib.BulkTransfer(d.handle, ep,
			uintptr(unsafe.Pointer(&p[0])), int32(len(p)),
			uintptr(unsafe.Pointer(&transferred)), transferTimeoutMs)
		if ret == 0 && int(transferred) == len(p) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("electronbot: bulk 写超时 ep=0x%x ret=%d transferred=%d", ep, ret, transferred)
		}
	}
}

// bulkReadExact 读满 p，必要时重试（对照官方 ReceivePacket：循环到收满为止），带总超时兜底。
func (d *Device) bulkReadExact(ep uint8, p []byte) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		var transferred int32
		ret := d.lib.BulkTransfer(d.handle, ep,
			uintptr(unsafe.Pointer(&p[0])), int32(len(p)),
			uintptr(unsafe.Pointer(&transferred)), transferTimeoutMs)
		if ret == 0 && int(transferred) == len(p) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("electronbot: bulk 读超时 ep=0x%x ret=%d transferred=%d", ep, ret, transferred)
		}
	}
}

// 编译期断言：Device 实现 robot.Transport。
var _ robot.Transport = (*Device)(nil)
