//go:build windows || linux

package electronbot

// windows / linux 上的复位串口自动探测：go.bug.st/serial/enumerator 在这两个平台是纯 Go(无 cgo)，
// 能列出每个串口的 USB VID/PID，据此精确匹配 CP210x/CH340。darwin 的 enumerator 依赖 IOKit(cgo)、
// 与本项目 CGO_ENABLED=0 冲突，故不在此编译（见 reboot_detect_stub.go）。

import (
	"fmt"
	"strings"

	"go.bug.st/serial/enumerator"
)

// detectBridgePort 枚举串口、按 VID/PID 找 USB 转串口桥（CP210x/CH340），返回其端口名（如 "COM3"）。
func detectBridgePort(bridges []usbBridge) (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", fmt.Errorf("electronbot: 枚举串口失败: %w", err)
	}
	for _, port := range ports {
		if !port.IsUSB {
			continue
		}
		for _, b := range bridges {
			if strings.EqualFold(port.VID, b.vid) && strings.EqualFold(port.PID, b.pid) {
				return port.Name, nil
			}
		}
	}
	return "", fmt.Errorf("electronbot: 未找到复位串口(CP210x/CH340)——请确认设备已插好，或在配置里显式指定 io.reset_port")
}
