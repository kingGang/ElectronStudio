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
	}
}
