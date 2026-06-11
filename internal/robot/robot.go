// Package robot 定义机器人硬件传输层的抽象接口。
//
// 上层（动作编排、画面推送等）只依赖 Transport 接口，不关心底层是
// 真实的 ElectronBot（USB）还是树莓派直连（SPI）或 Mock。
// 真机实现见子包，如 internal/robot/electronbot。
package robot

const (
	// JointCount 为 ElectronBot 的舵机数量（6 轴）。
	JointCount = 6
	// ScreenWidth / ScreenHeight 为机器人圆形屏幕的像素尺寸。
	ScreenWidth  = 240
	ScreenHeight = 240
	// ImageBytesRGB888 为一帧 RGB888 画面应有的字节数。
	ImageBytesRGB888 = ScreenWidth * ScreenHeight * 3
)

// Joints 是 6 轴舵机角度（单位：度）。使用定长数组以避免误用和堆分配。
type Joints = [JointCount]float32

// Transport 抽象与机器人本体的通信。一次完整的"下发并读回"为：
//
//	SetImage(...)        // 准备画面（可选）
//	SetJointAngles(...)  // 准备舵机角度
//	Sync()               // 执行一次 USB 收发：下发画面+角度，读回真实角度
//	JointAngles()        // 取最近一次读回的真实角度
//
// 实现需保证并发安全或由调用方串行使用；本项目中由动作编排引擎串行驱动。
type Transport interface {
	// Connect 建立与机器人的连接。
	Connect() error
	// SetImage 设置下一帧要显示的画面，要求为 ImageBytesRGB888 字节的 RGB888 数据。
	SetImage(rgb888 []byte) error
	// SetJointAngles 设置目标舵机角度；enable 为 false 时舵机不上力（松弛）。
	SetJointAngles(angles Joints, enable bool) error
	// Sync 执行一次与机器人的数据交换（下发画面/角度，读回反馈）。
	Sync() error
	// JointAngles 返回最近一次 Sync 读回的真实舵机角度。
	JointAngles() Joints
	// Connected 报告当前是否已连接。
	Connected() bool
	// Close 释放连接资源。
	Close() error
}
