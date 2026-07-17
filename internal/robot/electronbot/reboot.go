package electronbot

// —— ElectronBot 的串口软复位（免拔电源）——
//
// ElectronBot 主板是"STM32 原生 USB(bulk，走 libusb/WinUSB，会卡死) + 一颗 USB 转串口桥接芯片
// (接 MCU 的 UART)"的组合，两者挂在机内同一颗 USB hub 上（精英版实测：WinUSB 口 5241:5241 与
// CP210x 串口共用 hub 214B:7250 的相邻端口）。bulk 主循环是无超时自旋、卡死就得复位；而串口那路由
// MCU 的串口中断独立处理，即便 bulk 卡死它照样活着。往串口写这条 11 字节指令即触发固件系统复位 →
// USB 重新枚举 → 恢复，全程不用拔电源。
//
// 对照官方 ElectronBot.DotNet 的 HomeViewModel.RebootElectron()：打开 CP210x/CH340 串口 @115200，
// 写这 11 字节，Sleep 1s 再关。真机(精英版)实测：发完后设备从总线断开(NO_DEVICE)、约 6s 后重新枚举，
// 驱动的掉线重连即自动接回。
//
// 串口端口的自动探测(按 VID/PID)分平台实现：windows/linux 用 go.bug.st/serial/enumerator(纯 Go)，
// 见 reboot_detect_enum.go；darwin 的枚举依赖 IOKit(cgo)、与本项目 CGO_ENABLED=0 冲突，故走 stub
// (见 reboot_detect_stub.go)、要求显式配置 io.reset_port。打开/写串口本身(下面的 serial.Open)在所有
// 平台都是纯 Go。

import (
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
)

// rebootCommand 是固件的串口软复位指令（逐字节对照官方 RebootElectron 的 byteData）。
// 0xEA 帧头/帧尾包裹；中段 0x0D 0x02 为命令，0x0F 疑似校验(0x0D ^ 0x02 = 0x0F)。
var rebootCommand = []byte{0xEA, 0x00, 0x00, 0x00, 0x00, 0x0D, 0x02, 0x00, 0x00, 0x0F, 0xEA}

// serialBaud 是复位串口的波特率（官方固定 115200，8N1）。
const serialBaud = 115200

// usbBridge 是一个 USB 转串口桥接芯片的 USB VID/PID（十六进制字符串，大写）。
type usbBridge struct{ vid, pid string }

// serialBridges 是 ElectronBot 主板上"接 MCU UART 的 USB 转串口芯片"的 USB VID/PID。
// 自动找复位串口时据此匹配（比官方按设备名匹配 "CP210"/"CH910" 更稳）。
//   - CP2102：10C4:EA60（精英版实测就是它）
//   - CH340 ：1A86:7523（初代常见）
var serialBridges = []usbBridge{
	{"10C4", "EA60"},
	{"1A86", "7523"},
}

// findResetPort 找 ElectronBot 的串口复位通道。configPort 非空且非 "auto" 时直接用它（如 "COM3"）；
// 否则交给平台相关的 detectBridgePort 按 VID/PID 自动匹配。
func findResetPort(configPort string) (string, error) {
	if p := strings.TrimSpace(configPort); p != "" && !strings.EqualFold(p, "auto") {
		return p, nil
	}
	return detectBridgePort(serialBridges)
}

// SendReboot 通过串口给 ElectronBot 发一次软复位指令（免拔电源）。
// configPort 可为 "" / "auto"（自动按 VID/PID 找 CP210x/CH340），或显式串口名（如 "COM3" / "/dev/ttyUSB0"）。
// 返回实际使用的串口名与错误。
func SendReboot(configPort string) (string, error) {
	name, err := findResetPort(configPort)
	if err != nil {
		return "", err
	}
	mode := &serial.Mode{
		BaudRate: serialBaud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(name, mode)
	if err != nil {
		return name, fmt.Errorf("electronbot: 打开复位串口 %s 失败: %w", name, err)
	}
	defer func() { _ = port.Close() }()
	if _, err := port.Write(rebootCommand); err != nil {
		return name, fmt.Errorf("electronbot: 写复位指令到 %s 失败: %w", name, err)
	}
	// 官方发完 Sleep 1s 再关端口——给 MCU 收完指令、开始复位的时间，别写完立刻关(可能截断未发完)。
	time.Sleep(1 * time.Second)
	return name, nil
}
