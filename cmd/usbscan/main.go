// usbscan 是一个独立的 USB 设备扫描小工具：列出当前主机上所有 USB 设备的
// VID:PID、设备类与厂商/产品字符串，并高亮疑似 ElectronBot 的条目。
//
// 用途：排查真机连不上时，先确认设备到底有没有枚举出来、以及它真实的 VID/PID
// 是多少（精英版“免驱(WinUSB)”固件的 VID/PID 可能与代码里写死的 0x1001:0x8023 不同）。
//
// 纯 Go、CGO_ENABLED=0：运行时通过 purego 动态加载 libusb（与 internal/robot/electronbot 一致）。
//
//	CGO_ENABLED=0 go run ./cmd/usbscan
//
// macOS 需先 `brew install libusb`；Linux 装 libusb-1.0；Windows 放 libusb-1.0.dll。
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// 代码里期望的 ElectronBot 标识（见 internal/robot/electronbot/protocol.go）。
const (
	ebVID = 0x1001
	ebPID = 0x8023
)

// libusb_device_descriptor 的内存布局（与 C 结构体逐字段对齐一致，共 18 字节）。
type devDesc struct {
	bLength            uint8
	bDescriptorType    uint8
	bcdUSB             uint16
	bDeviceClass       uint8
	bDeviceSubClass    uint8
	bDeviceProtocol    uint8
	bMaxPacketSize0    uint8
	idVendor           uint16
	idProduct          uint16
	bcdDevice          uint16
	iManufacturer      uint8
	iProduct           uint8
	iSerialNumber      uint8
	bNumConfigurations uint8
}

type libusb struct {
	Init           func(ctx uintptr) int32
	Exit           func(ctx uintptr)
	GetDeviceList  func(ctx uintptr, list uintptr) int64
	FreeDeviceList func(list uintptr, unref int32)
	GetDeviceDesc  func(dev uintptr, desc uintptr) int32
	GetBusNumber   func(dev uintptr) uint8
	GetDevAddress  func(dev uintptr) uint8
	GetPortNumber  func(dev uintptr) uint8
	GetParent      func(dev uintptr) uintptr
	Open           func(dev uintptr, handle uintptr) int32
	Close          func(handle uintptr)
	GetStringASCII func(handle uintptr, index uint8, data uintptr, length int32) int32
	GetActiveCfg   func(dev uintptr, cfg uintptr) int32
	FreeConfig     func(cfg uintptr)
	ControlXfer    func(handle uintptr, reqType uint8, req uint8, value, index uint16, data uintptr, length uint16, timeout uint32) int32
}

// libusb_config_descriptor 前 8 字节即原始配置描述符；bmAttributes@6、MaxPower@7。
// MaxPower 单位 2mA；bmAttributes bit6(0x40)=自供电。
type cfgDesc struct {
	bLength             uint8
	bDescriptorType     uint8
	wTotalLength        uint16
	bNumInterfaces      uint8
	bConfigurationValue uint8
	iConfiguration      uint8
	bmAttributes        uint8
	maxPower            uint8
	// 其后是指针字段，这里不需要，不声明也无妨（只读前 8 字节）。
}

// powerInfo 读取设备配置描述符，返回 "总线供电/自供电 + 声明电流"。无需 open 设备。
func powerInfo(l *libusb, dev uintptr) string {
	var cfgPtr unsafe.Pointer
	if l.GetActiveCfg(dev, uintptr(unsafe.Pointer(&cfgPtr))) != 0 || cfgPtr == nil {
		return ""
	}
	defer l.FreeConfig(uintptr(cfgPtr))
	c := (*cfgDesc)(cfgPtr)
	kind := "总线供电"
	if c.bmAttributes&0x40 != 0 {
		kind = "自供电"
	}
	return fmt.Sprintf("%s, 声明 %dmA", kind, int(c.maxPower)*2)
}

func candidateLibs() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"libusb-1.0.dll"}
	case "darwin":
		return []string{"libusb-1.0.0.dylib", "libusb-1.0.dylib"}
	default:
		return []string{"libusb-1.0.so.0", "libusb-1.0.so"}
	}
}

func load() (*libusb, error) {
	var lastErr error
	for _, name := range candidateLibs() {
		h, err := dlopen(name) // 平台相关：Unix 走 purego.Dlopen，Windows 走 LoadLibrary
		if err != nil {
			lastErr = err
			continue
		}
		l := &libusb{}
		purego.RegisterLibFunc(&l.Init, h, "libusb_init")
		purego.RegisterLibFunc(&l.Exit, h, "libusb_exit")
		purego.RegisterLibFunc(&l.GetDeviceList, h, "libusb_get_device_list")
		purego.RegisterLibFunc(&l.FreeDeviceList, h, "libusb_free_device_list")
		purego.RegisterLibFunc(&l.GetDeviceDesc, h, "libusb_get_device_descriptor")
		purego.RegisterLibFunc(&l.GetBusNumber, h, "libusb_get_bus_number")
		purego.RegisterLibFunc(&l.GetDevAddress, h, "libusb_get_device_address")
		purego.RegisterLibFunc(&l.GetPortNumber, h, "libusb_get_port_number")
		purego.RegisterLibFunc(&l.GetParent, h, "libusb_get_parent")
		purego.RegisterLibFunc(&l.Open, h, "libusb_open")
		purego.RegisterLibFunc(&l.Close, h, "libusb_close")
		purego.RegisterLibFunc(&l.GetStringASCII, h, "libusb_get_string_descriptor_ascii")
		purego.RegisterLibFunc(&l.GetActiveCfg, h, "libusb_get_active_config_descriptor")
		purego.RegisterLibFunc(&l.FreeConfig, h, "libusb_free_config_descriptor")
		purego.RegisterLibFunc(&l.ControlXfer, h, "libusb_control_transfer")
		return l, nil
	}
	return nil, fmt.Errorf("无法加载 libusb（尝试 %v）: %w（macOS 请先 brew install libusb）", candidateLibs(), lastErr)
}

// readString 尝试打开设备读取一个 ASCII 字符串描述符；失败（常因权限/被占用）返回空串。
func readString(l *libusb, dev uintptr, index uint8) string {
	if index == 0 {
		return ""
	}
	var handle uintptr
	if l.Open(dev, uintptr(unsafe.Pointer(&handle))) != 0 || handle == 0 {
		return ""
	}
	defer l.Close(handle)
	var buf [256]byte
	n := l.GetStringASCII(handle, index, uintptr(unsafe.Pointer(&buf[0])), int32(len(buf)))
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

// dumpEndpoints 打开设备、用控制传输读原始配置描述符，解析并打印接口与端点布局。
// 用于核对精英版主控的端点（与初代 EP 0x01/0x81 对比）。失败返回错误说明。
func dumpEndpoints(l *libusb, dev uintptr) string {
	var handle uintptr
	if l.Open(dev, uintptr(unsafe.Pointer(&handle))) != 0 || handle == 0 {
		return "（无法打开设备读端点：可能被占用，先停掉 electronstudio 再试）"
	}
	defer l.Close(handle)
	buf := make([]byte, 512)
	// GET_DESCRIPTOR(CONFIGURATION, index 0)
	n := l.ControlXfer(handle, 0x80, 0x06, 0x0200, 0, uintptr(unsafe.Pointer(&buf[0])), uint16(len(buf)), 1000)
	if n < 4 {
		return fmt.Sprintf("（读配置描述符失败 ret=%d）", n)
	}
	b := buf[:n]
	var out []string
	dirType := func(addr, attr uint8) string {
		dir := "OUT"
		if addr&0x80 != 0 {
			dir = "IN"
		}
		t := []string{"控制", "同步", "bulk", "中断"}[attr&0x03]
		return fmt.Sprintf("EP %#04x (%s, %s)", addr, dir, t)
	}
	for o := 0; o+2 <= len(b); {
		ln := int(b[o])
		if ln < 2 || o+ln > len(b) {
			break
		}
		switch b[o+1] {
		case 0x04: // INTERFACE
			out = append(out, fmt.Sprintf("接口 #%d: 类=%#04x 端点数=%d", b[o+2], b[o+5], b[o+4]))
		case 0x05: // ENDPOINT
			mps := int(b[o+4]) | int(b[o+5])<<8
			out = append(out, fmt.Sprintf("    %s  最大包=%d", dirType(b[o+2], b[o+3]), mps))
		}
		o += ln
	}
	if len(out) == 0 {
		return "（未解析到接口/端点）"
	}
	return "\n      " + strings.Join(out, "\n      ")
}

// usbClassName 给常见设备类一个可读名字（仅作排错参考）。
func usbClassName(c uint8) string {
	switch c {
	case 0x00:
		return "（看接口）"
	case 0x01:
		return "音频"
	case 0x02:
		return "通信/CDC"
	case 0x03:
		return "HID"
	case 0x08:
		return "存储"
	case 0x09:
		return "Hub"
	case 0x0a:
		return "CDC-Data"
	case 0x0e:
		return "视频"
	case 0xef:
		return "杂项(IAD)"
	case 0xff:
		return "厂商自定义(WinUSB常见)"
	default:
		return fmt.Sprintf("0x%02x", c)
	}
}

func main() {
	l, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
	if r := l.Init(0); r != 0 {
		fmt.Fprintf(os.Stderr, "✗ libusb_init 失败: %d\n", r)
		os.Exit(1)
	}
	defer l.Exit(0)

	var listPtr unsafe.Pointer
	count := l.GetDeviceList(0, uintptr(unsafe.Pointer(&listPtr)))
	if count < 0 {
		fmt.Fprintf(os.Stderr, "✗ libusb_get_device_list 失败: %d\n", count)
		os.Exit(1)
	}
	defer l.FreeDeviceList(uintptr(listPtr), 1)

	// 先把所有设备收进切片，便于解析 Hub 父子拓扑。
	ptrSize := unsafe.Sizeof(uintptr(0))
	type entry struct {
		dev, parent uintptr
		d           devDesc
		bus, addr   uint8
		port        uint8
		mfr, prod   string
		power       string
	}
	var devs []entry
	for i := range int(count) {
		dev := *(*uintptr)(unsafe.Add(listPtr, uintptr(i)*ptrSize))
		if dev == 0 {
			break
		}
		var d devDesc
		if l.GetDeviceDesc(dev, uintptr(unsafe.Pointer(&d))) != 0 {
			continue
		}
		devs = append(devs, entry{
			dev:    dev,
			parent: l.GetParent(dev),
			d:      d,
			bus:    l.GetBusNumber(dev),
			addr:   l.GetDevAddress(dev),
			port:   l.GetPortNumber(dev),
			mfr:    readString(l, dev, d.iManufacturer),
			prod:   readString(l, dev, d.iProduct),
			power:  powerInfo(l, dev),
		})
	}
	addrOf := func(p uintptr) int {
		for _, e := range devs {
			if e.dev == p {
				return int(e.addr)
			}
		}
		return -1
	}

	fmt.Printf("发现 %d 个 USB 设备：\n\n", len(devs))
	var foundMain bool // 主控（WinUSB / 1001:8023）
	var ebHubs []int   // 疑似 ElectronBot 语音板内部 Hub 的 addr
	for _, e := range devs {
		d := e.d
		mark, note := "  ", ""
		isMain := false
		switch {
		case d.idVendor == ebVID && d.idProduct == ebPID:
			mark, note, foundMain, isMain = "➜ ", "  ★ 主控！初代 ElectronBot (1001:8023)", true, true
		case d.idVendor == 0x5241 && d.idProduct == 0x5241:
			mark, note, foundMain, isMain = "➜ ", "  ★ 主控！精英版 ElectronBot Elite (5241:5241)", true, true
		case d.idVendor == 0x214b: // 语音板内部 Hub（实测精英版语音板用此 Hub）
			mark, note = "● ", "  ← ElectronBot 语音板的内部 Hub"
			ebHubs = append(ebHubs, int(e.addr))
		case d.idVendor == 0x08bb: // Burr-Brown/TI USB 声卡 = 语音板的麦克风/扬声器
			mark, note = "● ", "  ← ElectronBot 语音板的 USB 声卡（麦/扬声器）"
		case d.idVendor == 0x0483: // STMicroelectronics
			mark, note, foundMain = "? ", "  ← STM32 厂商，疑似精英版主控（VID/PID 与代码不符）", true
		case d.bDeviceClass == 0xff:
			mark, note = "? ", "  ← 厂商自定义类(WinUSB 主控常长这样)"
		}
		parent := ""
		if pa := addrOf(e.parent); pa >= 0 {
			parent = fmt.Sprintf(" 挂在 addr %d 的口%d 上", pa, e.port)
		}
		fmt.Printf("%s[bus %d addr %d]%s %04x:%04x  类=%s  bcdUSB=%#04x%s\n",
			mark, e.bus, e.addr, parent, d.idVendor, d.idProduct, usbClassName(d.bDeviceClass), d.bcdUSB, note)
		if e.mfr != "" || e.prod != "" {
			fmt.Printf("      厂商=%q  产品=%q\n", e.mfr, e.prod)
		}
		if e.power != "" {
			fmt.Printf("      供电: %s\n", e.power)
		}
		if isMain {
			fmt.Printf("      端点布局:%s\n", dumpEndpoints(l, e.dev))
		}
	}

	fmt.Println()
	switch {
	case foundMain:
		fmt.Println("✓ 主控已枚举。把上面标 ➜/? 那行的 VID:PID 发我；若不是 1001:8023 我改代码适配即可。")
	case len(ebHubs) > 0:
		fmt.Printf("◑ 语音板已枚举（Hub/声卡在线，addr %v），但【主控没出现】。\n", ebHubs)
		fmt.Println("  语音板供电够、能枚举；主控（屏幕+舵机，吃电大）挂在它的 Hub 后面却没起来。")
		fmt.Println("  高度指向：① 主控↔语音板排针没插实  ② 整链供电不足（给语音板 Type-C 单独接充电器/有源 Hub 补电）。")
	default:
		fmt.Println("✗ 连语音板都没枚举 → 物理层没通（线/口/供电），先解决连接再说。")
	}
}
