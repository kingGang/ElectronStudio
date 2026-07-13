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
	"fmt"
	"math"
	"strings"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// USB 设备标识与端点。
const (
	endpointIn  = 0x81 // EP1_IN（读：MCU 反馈）
	endpointOut = 0x01 // EP1_OUT（写：图像 + 舵机角度）
	ifaceNum    = 0    // 接口号
)

// deviceID 是一组受支持的 USB 标识。
type deviceID struct {
	vid, pid uint16
	name     string
}

// supportedDevices 列出已知的 ElectronBot 设备标识。连接时依次尝试。
//   - 初代 ElectronBot：0x1001:0x8023（官方 SDK 标识）。
//   - 精英版 ElectronBot Elite：0x5241:0x5241（免驱 WinUSB 固件，实测自 USB Tree Viewer）。
//
// 两者共用同一套 Sync 低层协议（4 段、512 字节包、EP 0x01/0x81）；精英版若端点不符
// 需在此基础上再调整。
var supportedDevices = []deviceID{
	{0x1001, 0x8023, "ElectronBot"},
	{0x5241, 0x5241, "ElectronBot Elite"},
}

// deviceListString 返回受支持设备的可读列表，用于错误信息。
func deviceListString() string {
	parts := make([]string, len(supportedDevices))
	for i, d := range supportedDevices {
		parts[i] = fmt.Sprintf("%s(%04x:%04x)", d.name, d.vid, d.pid)
	}
	return strings.Join(parts, ", ")
}

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

	// readTimeoutMs：【首帧】读 32 字节就绪包的超时（对照官方 ReceivePacket 的 5000ms）。
	// 刚连上时设备可能还没进入收发循环（连接后就绪窗口），必须等足，绝不能提前放弃。
	readTimeoutMs = 5000
	// readTimeoutSteadyMs：【稳态】读请求包的超时。跑起来之后设备每 ~8ms 就发一个请求包
	// （30fps × 4 段），迟迟不来基本就是这个包被同一 hub 上的等时音频流挤掉了——此时越早认定
	// "丢了、直接发图接回 lockstep"越好（见 bulkRetry）。
	//
	// 【为什么不沿用 5000】：官方之所以敢用 5s，是因为它超时后无限重读、根本不指望自愈；而我们
	// 靠超时来【判定丢包】，5s 就意味着每丢一个包，整个同步循环白等 5 秒——设备屏和网页镜像
	// 一起冻住 5 秒。真机浸泡：10 分钟触发 15 次，等于每 40 秒卡一下 5 秒，肉眼非常明显。
	// 500ms 已是正常响应时延(~8ms)的 60 倍余量，既不会误判，卡顿也降到几乎无感。
	readTimeoutSteadyMs = 500
	writeTimeoutMs      = 2000 // 写 bulk 包的超时（对照官方 TransmitPacket）
)

// 编译期校验：整帧字节数须与屏幕尺寸及通用常量一致。
var _ = [1]struct{}{}[imageBytes-robot.ImageBytesRGB888] // imageBytes == 240*240*3

// buildExtraData 把使能位与 6 轴角度打包为 32 字节 extraData（小端 float32）。
// 布局：[0]=使能(0/1)，[1+4j .. 4]=第 j 轴角度，共 6 轴占 24 字节。
//
// 与官方 SDK 逐字节一致（ElectronBot.DotNet 的 SetJointAngles）：【任何时候都发真实角度】，
// enable 位只决定舵机上不上扭矩，不影响角度字段。
//
// 曾经这里在 enable=false 时把 6 个角度全发 NaN，好让固件的
//
//	if (setPoint >= angleMin && setPoint <= angleMax) { TransmitAndReceiveI2cPacket(...) }
//
// 恒为假、从而完全跳过舵机 I2C。那是我们自己发明的、官方没有的行为，副作用很致命：舵机长期收不到
// 任何 setpoint，一旦 enable 由 0 跳到 1，固件立刻上扭矩并给出一个与当前位置相距很远的目标 →
// 电流尖峰 → 舵机不 ACK → 固件 I2C 是 do{...}while(state != HAL_OK)（无超时无限重试）→ 永久自旋
// → 不再发 USB 反馈 → 主控硬死（只能断电复位）。官方持续下发 setpoint，舵机始终平滑跟随，没有这个跳变。
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
		v := math.Float32frombits(binary.LittleEndian.Uint32(buf[1+4*j:]))
		// 舵机禁用(发 NaN)时固件不走 I2C 读舵机，反馈角度字段是未初始化垃圾值，
		// 偶尔为 NaN/±Inf。钳成 0，避免污染 UI 关节显示与 JSON 广播(json 无法编码非有限值)。
		if f := float64(v); math.IsNaN(f) || math.IsInf(f, 0) {
			v = 0
		}
		a[j] = v
	}
	return a
}
