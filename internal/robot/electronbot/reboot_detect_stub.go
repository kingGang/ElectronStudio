//go:build !windows && !linux

package electronbot

// 非 windows/linux 平台（主要是 macOS）的复位串口探测桩：go.bug.st/serial/enumerator 在 darwin
// 依赖 IOKit(cgo)，与本项目 CGO_ENABLED=0 冲突，故这里不做自动枚举，要求用户在配置里显式指定
// io.reset_port（如 "/dev/cu.SLAB_USBtoUART"）。打开/写串口本身(serial.Open)在 darwin 仍是纯 Go、照常工作。

import "fmt"

// detectBridgePort 在本平台不支持按 VID/PID 自动探测——返回提示，让用户显式配置 io.reset_port。
func detectBridgePort(_ []usbBridge) (string, error) {
	return "", fmt.Errorf("electronbot: 本平台不支持自动探测复位串口，请在配置里显式指定 io.reset_port（如 /dev/cu.SLAB_USBtoUART）")
}
