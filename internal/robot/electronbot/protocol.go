// Package electronbot 实现 robot.Transport，通过 USB 直连稚晖君的 ElectronBot 桌面机器人。
//
// 通信协议严格对照官方低层 SDK（电子骨 ElectronBotSDK-LowLevel 的 electron_low_level.cpp）：
//
//	设备：VID 0x1001 / PID 0x8023；bulk 端点 EP1_OUT(0x01) 写、EP1_IN(0x81) 读。
//	一次 Sync = 循环 4 段，每段：
//	    1) 从 EP1_IN 读 32 字节（MCU 请求 + 舵机角度反馈）
//	    2) 向 EP1_OUT 写 84 个 512 字节包（= 43008 字节图像数据）
//	    3) 向 EP1_OUT 写 1 个 224 字节包（= 192 字节图像尾 + 32 字节 extraData）
//	图像为 240×240×3 = 172800 字节 RGB888；extraData 32 字节 = 1 字节使能 + 6×float32 舵机角度。
//
// 为保持主程序纯 Go（无 cgo、可交叉编译），这里用 purego 在运行时动态加载 libusb，
// 而非链接官方的 Windows 专用 USBInterface.dll。
package electronbot

import (
	"encoding/binary"
	"math"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// USB 设备标识与端点。
const (
	vid         = 0x1001 // USB 厂商 ID
	pid         = 0x8023 // USB 产品 ID
	endpointIn  = 0x81   // EP1_IN（读：MCU 反馈）
	endpointOut = 0x01   // EP1_OUT（写：图像 + 舵机角度）
	ifaceNum    = 0      // 接口号
)

// 帧分段参数（与官方 SyncTask 完全一致）。
const (
	segments        = 4                              // 分段数
	packetsPerSeg   = 84                             // 每段图像包数
	packetSize      = 512                            // 每包字节
	segImageBytes   = packetsPerSeg * packetSize     // 每段图像主体 = 43008
	tailImageBytes  = 192                            // 每段图像尾
	extraDataBytes  = 32                             // extraData 长度
	tailPacketBytes = tailImageBytes + extraDataBytes // 尾包 = 224
	segStride       = segImageBytes + tailImageBytes  // 每段图像总长 = 43200
	imageBytes      = segments * segStride            // 整帧图像 = 172800

	transferTimeoutMs = 100 // 单次 bulk 传输超时（毫秒），与官方一致
)

// 编译期校验：整帧字节数须与屏幕尺寸及通用常量一致。
var _ = [1]struct{}{}[imageBytes-robot.ImageBytesRGB888] // imageBytes == 240*240*3

// buildExtraData 把使能位与 6 轴角度打包为 32 字节 extraData（小端 float32）。
// 布局：[0]=使能(0/1)，[1+4j .. 4]=第 j 轴角度，共 6 轴占 24 字节。
func buildExtraData(angles robot.Joints, enable bool) [extraDataBytes]byte {
	var b [extraDataBytes]byte
	if enable {
		b[0] = 1
	}
	for j := 0; j < robot.JointCount; j++ {
		binary.LittleEndian.PutUint32(b[1+4*j:], math.Float32bits(angles[j]))
	}
	return b
}

// parseFeedback 从 32 字节反馈中解析 6 轴真实角度（布局同 buildExtraData）。
func parseFeedback(buf []byte) robot.Joints {
	var a robot.Joints
	if len(buf) < extraDataBytes {
		return a
	}
	for j := 0; j < robot.JointCount; j++ {
		a[j] = math.Float32frombits(binary.LittleEndian.Uint32(buf[1+4*j:]))
	}
	return a
}
