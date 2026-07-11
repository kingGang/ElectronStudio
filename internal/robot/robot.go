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

// 6 轴关节索引，顺序即官方下发给固件的线上顺序：头排在 0 号。
// 依据官方上位机 3.Software/Unity/ElectronBot-Studio/Assets/Scripts/UnityGetImageFromCpp.cs：
//
//	joints[0] = sliderAngleHead;          joints[1] = sliderAngleArmRollLeft;
//	joints[2] = sliderAngleArmPitchLeft;  joints[3] = sliderAngleArmRollRight;
//	joints[4] = sliderAngleArmPitchRight; joints[5] = sliderAngleBody;
//
// （别照 RobotController.cs 的字段声明顺序——那不是线上顺序，照抄会让头/手臂整体错位一格。）
// 注意：ElectronBot 为 6 自由度，每臂 2 个（横滚 + 俯仰），头 1 个（俯仰），身体 1 个（偏航）——没有肘关节。
const (
	JointHead          = 0 // 头部俯仰（X 轴）
	JointArmRollLeft   = 1 // 左臂横滚（Z 轴）
	JointArmPitchLeft  = 2 // 左臂俯仰（X 轴）
	JointArmRollRight  = 3 // 右臂横滚（Z 轴，模型中取反）
	JointArmPitchRight = 4 // 右臂俯仰（X 轴）
	JointBody          = 5 // 身体旋转 / 偏航（Y 轴）
)

// JointNames 是 6 轴的中文名，下标与 Joints 一致。
var JointNames = [JointCount]string{"头部俯仰", "左臂横滚", "左臂俯仰", "右臂横滚", "右臂俯仰", "身体旋转"}

// JointLimits 是各轴允许的角度范围(度)[min,max]，与 JointNames 同序。
// 来自 ElectronBot 官方设定（= 固件 joint 表里的 modelAngelMin/Max）：头 -15~15、横滚 0~30、
// 俯仰 -20~180、身体 -90~90。
//
// 右臂俯仰上限收到 160：固件那张模型角↔舵机角映射表标着 "Need to adjust parameters for specific
// hardware"，精英版的机械限位比它假设的紧——真机实测 167 就顶死了，取 160 留 7° 余量。顶死=堵转=
// 电流尖峰，会让舵机不 ACK、固件 I²C 无限重试自旋，进而主控硬死（只能断电复位）。宁可少 20°，
// 也不能让上层（滑杆/动作编排）命令得到那个位置。
var JointLimits = [JointCount][2]float32{{-15, 15}, {0, 30}, {-20, 180}, {0, 30}, {-20, 160}, {-90, 90}}

// ClampAngle 把第 i 轴角度裁剪到允许范围内。
func ClampAngle(i int, a float32) float32 {
	if i < 0 || i >= JointCount {
		return a
	}
	lo, hi := JointLimits[i][0], JointLimits[i][1]
	if a < lo {
		return lo
	}
	if a > hi {
		return hi
	}
	return a
}

// ClampJoints 把整组角度逐轴裁剪到允许范围。
func ClampJoints(j Joints) Joints {
	for i := range j {
		j[i] = ClampAngle(i, j[i])
	}
	return j
}

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
	// Speed 返回 USB 连接速度("USB 2.0"/"USB 3.0" 等)；Mock 或未连接时为空。
	Speed() string
	// Close 释放连接资源。
	Close() error
}
