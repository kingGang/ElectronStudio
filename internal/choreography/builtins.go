package choreography

import "github.com/kingGang/ElectronStudio/internal/robot"

// 本文件定义内置动作。关节顺序严格对照官方 ElectronStudio 的 RobotController：
//
//	J0 左臂横滚  J1 左臂俯仰  J2 右臂横滚  J3 右臂俯仰  J4 头部俯仰  J5 身体旋转
//
// 角度为示意值（度）。关节"用哪个轴"是确定的（与官方一致），但具体正负方向与
// 幅度受真机舵机装配方向/量程影响，真机到货后可微调（仅调数值，不动结构）。

// j 按 J0..J5 顺序构造 robot.Joints。
func j(armRollL, armPitchL, armRollR, armPitchR, head, body float32) robot.Joints {
	return robot.Joints{armRollL, armPitchL, armRollR, armPitchR, head, body}
}

// home 是中位（全部归零）姿态。
var home = robot.Joints{}

// danceBPM 是内置舞蹈 dance 的速度（每分钟拍数）。动作关键帧用 bt(拍) 表达，
// 改这一个常量即可整体重定速以匹配不同歌曲（120 → 500ms/拍，8 拍一个乐句 = 4s）。
const danceBPM = 120

// bt 把"第几拍"（可含半拍小数）换算成毫秒，用于让关键帧严格落在节拍点上。
func bt(beats float64) int { return int(beats*60000/danceBPM + 0.5) }

// DefaultActions 返回一组内置动作。
func DefaultActions() []Action {
	return []Action{
		// 归位。
		{
			Name:    "home",
			Emotion: "neutral",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 500, Angles: home},
			},
		},
		// 打招呼：抬起右臂（俯仰）并左右摆动（横滚）。
		{
			Name:    "wave",
			Emotion: "happy",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 300, Angles: j(0, 0, 40, -60, 0, 0)},
				{AtMs: 600, Angles: j(0, 0, -40, -60, 0, 0)},
				{AtMs: 900, Angles: j(0, 0, 40, -60, 0, 0)},
				{AtMs: 1200, Angles: j(0, 0, -40, -60, 0, 0)},
				{AtMs: 1500, Angles: home},
			},
		},
		// 点头：头部俯仰上下（J4）。
		{
			Name:    "nod",
			Emotion: "neutral",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 200, Angles: j(0, 0, 0, 0, 20, 0)},
				{AtMs: 400, Angles: j(0, 0, 0, 0, -10, 0)},
				{AtMs: 600, Angles: j(0, 0, 0, 0, 20, 0)},
				{AtMs: 800, Angles: home},
			},
		},
		// 摇头：身体左右旋转（J5，头只能俯仰故用身体偏航表示否定）。
		{
			Name:    "shake",
			Emotion: "confused",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 200, Angles: j(0, 0, 0, 0, 0, 25)},
				{AtMs: 400, Angles: j(0, 0, 0, 0, 0, -25)},
				{AtMs: 600, Angles: j(0, 0, 0, 0, 0, 25)},
				{AtMs: 800, Angles: home},
			},
		},
		// 万岁：双臂同时抬起。
		{
			Name:    "cheer",
			Emotion: "happy",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 300, Angles: j(0, -70, 0, -70, -15, 0)},
				{AtMs: 700, Angles: j(0, -70, 0, -70, 15, 0)},
				{AtMs: 1100, Angles: j(0, -70, 0, -70, -15, 0)},
				{AtMs: 1500, Angles: home},
			},
		},
		// 跳舞：按 danceBPM 踩拍编排的一段 8 拍乐句，自带「表情轨道」（踩拍变脸）。
		// 配 ~120BPM 的歌、同时手动触发播放音乐 + 播放 dance，节拍即可对齐。
		// 角度沿用内置动作的舒展幅度（俯仰负=抬臂、横滚≤30、头±15内、身体偏航摆动）；
		// 真机若方向/幅度需修，只改这里的数值，结构与拍点不动。
		{
			Name:    "dance",
			Emotion: "happy",
			Loops:   4, // 跳 4 个乐句（约 16 秒 @120BPM）；UI 可传入其它循环次数
			Frames: []Keyframe{
				// 乐句开始：起势，双臂半举外展（踩拍变脸 → happy）
				{AtMs: bt(0), Angles: j(15, -30, 15, -30, 5, 0), Emotion: "happy"},
				{AtMs: bt(0.5), Angles: j(15, -28, 15, -28, -6, 12)}, // 半拍下沉律动
				// 拍2：身体右转 + 右臂上挥
				{AtMs: bt(1), Angles: j(12, -22, 30, -68, 8, 35)},
				{AtMs: bt(1.5), Angles: j(14, -26, 22, -50, 0, 18)},
				// 拍3：身体左转 + 左臂上挥（变脸 → cool 耍帅）
				{AtMs: bt(2), Angles: j(30, -68, 12, -22, 8, -35), Emotion: "cool"},
				{AtMs: bt(2.5), Angles: j(22, -50, 14, -26, 0, -18)},
				// 拍4：回中 → 下蹲蓄力（低头）准备高潮
				{AtMs: bt(3), Angles: j(16, -30, 16, -30, 6, 0)},
				{AtMs: bt(3.5), Angles: j(18, -45, 18, -45, -8, 0)},
				// 拍5：万岁高潮，双臂高举 + 抬头（变脸 → surprised 惊喜）
				{AtMs: bt(4), Angles: j(20, -70, 20, -70, 12, 0), Emotion: "surprised"},
				{AtMs: bt(4.5), Angles: j(20, -70, 20, -70, 12, 16)}, // 高举左右摆胯
				{AtMs: bt(5), Angles: j(20, -70, 20, -70, 12, -16)},
				// 拍6：收臂低头收力
				{AtMs: bt(5.5), Angles: j(12, -16, 12, -16, -10, 0)},
				// 拍7：身体右摆 + 右手招两下（变脸 → happy）
				{AtMs: bt(6), Angles: j(6, -16, 28, -60, 6, 30), Emotion: "happy"},
				{AtMs: bt(6.5), Angles: j(6, -16, 30, -68, 6, 30)},
				// 拍8：身体左摆 + 左手招两下
				{AtMs: bt(7), Angles: j(28, -60, 6, -16, 6, -30)},
				{AtMs: bt(7.5), Angles: j(30, -68, 6, -16, 6, -30)},
				// 回到起势，衔接下一乐句（循环无缝）
				{AtMs: bt(8), Angles: j(15, -30, 15, -30, 5, 0)},
			},
		},
	}
}
