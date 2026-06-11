package choreography

import "github.com/kingGang/ElectronStudio/internal/robot"

// 本文件定义内置动作。角度为示意值（度），实际幅度需结合真机舵机量程标定，
// 此处以"回到中位 → 动作 → 回到中位"的对称结构给出可直接运行的样例。
//
// 关节约定（沿用 ElectronBot 6 轴，仅作占位语义，可按真机调整）：
//
//	J0 头部旋转  J1 头部俯仰  J2 左臂  J3 左肘  J4 右臂  J5 右肘

// j 是构造 robot.Joints 的简写。
func j(v0, v1, v2, v3, v4, v5 float32) robot.Joints {
	return robot.Joints{v0, v1, v2, v3, v4, v5}
}

// home 是中位（全部归零）姿态，常用作动作的起止帧。
var home = robot.Joints{}

// DefaultActions 返回一组内置动作，供引擎注册。
func DefaultActions() []Action {
	return []Action{
		// 归位：缓慢回到中位。
		{
			Name:    "home",
			Emotion: "neutral",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 600, Angles: home},
			},
		},
		// 打招呼：右臂抬起并左右摆动两下。
		{
			Name:    "wave",
			Emotion: "happy",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 250, Angles: j(0, 0, 0, 0, 70, -20)},
				{AtMs: 500, Angles: j(0, 0, 0, 0, 70, 20)},
				{AtMs: 750, Angles: j(0, 0, 0, 0, 70, -20)},
				{AtMs: 1000, Angles: j(0, 0, 0, 0, 70, 20)},
				{AtMs: 1300, Angles: home},
			},
		},
		// 点头：头部俯仰上下两次。
		{
			Name:    "nod",
			Emotion: "neutral",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 200, Angles: j(0, 20, 0, 0, 0, 0)},
				{AtMs: 400, Angles: j(0, -10, 0, 0, 0, 0)},
				{AtMs: 600, Angles: j(0, 20, 0, 0, 0, 0)},
				{AtMs: 800, Angles: home},
			},
		},
		// 摇头：头部左右旋转表示否定。
		{
			Name:    "shake",
			Emotion: "confused",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 200, Angles: j(25, 0, 0, 0, 0, 0)},
				{AtMs: 400, Angles: j(-25, 0, 0, 0, 0, 0)},
				{AtMs: 600, Angles: j(25, 0, 0, 0, 0, 0)},
				{AtMs: 800, Angles: home},
			},
		},
	}
}
