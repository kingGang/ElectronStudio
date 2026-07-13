package choreography

import "github.com/kingGang/ElectronStudio/internal/robot"

// 本文件定义内置动作。写关键帧前先记住真机的【实际量程与方向】（robot.JointLimits，真机实测）：
//
//	俯仰 -20~180(右臂封顶 160)：0=手臂垂在身侧，角度越大越往上抬，150 左右就是高举过头。
//	横滚 0~30                ：0=贴身，30=向外张开（只有正方向，别写负数）。
//	头部 -15~15、身体 -90~90 。
//
// 【曾经的坑】老版本把"抬臂"写成负角度（-60、-70），而俯仰的下限是 -20 —— 全被 ClampAngle 掐成
// -20，胳膊几乎不动，跳起舞来毫无观赏性。所以：抬臂一律用【大正角度】。
//
// 【第二个坑】幅度越大，舵机走完越费时间。大动作只放在【整拍】（120BPM 下 500ms 一次，够舵机走完
// 130° 左右），半拍只做身体/头的小幅点缀 —— 否则舵机追不上关键帧，动作糊成一团、看着更"滞后"。
//
// 【第三个坑：撞头】限位管不住的一条——【胳膊高举时把横滚收回来 = 把手往脑袋上招】。所以只要
// 俯仰 ≥100，横滚就不许低于 ~18（保持向外张开）。俯仰本身也已全局收到 150（160 就会碰头）。

// j 按 J0..J5 顺序构造 robot.Joints（线上顺序：头在 0 号，见 robot.JointHead 一族常量）。
// 形参仍按"左臂→右臂→头→身体"的书写习惯排列，便于下面的关键帧读写；装配时才换成线上顺序。
func j(armRollL, armPitchL, armRollR, armPitchR, head, body float32) robot.Joints {
	return robot.Joints{head, armRollL, armPitchL, armRollR, armPitchR, body}
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
		// 打招呼：右臂高举过头，横滚来回张合地招手。
		{
			Name:    "wave",
			Emotion: "happy",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 450, Angles: j(0, 0, 30, 140, 6, 0)},  // 举起来（大动作给足时间）
				{AtMs: 750, Angles: j(0, 0, 20, 140, 6, 0)},  // 招一下（横滚不低于 18：举着还往里收会撞头）
				{AtMs: 1050, Angles: j(0, 0, 30, 140, 6, 0)}, // 招两下
				{AtMs: 1350, Angles: j(0, 0, 20, 140, 6, 0)},
				{AtMs: 1900, Angles: home}, // 放下
			},
		},
		// 点头：头部俯仰上下（量程只有 ±15，全用满）。
		{
			Name:    "nod",
			Emotion: "neutral",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 200, Angles: j(0, 0, 0, 0, 15, 0)},
				{AtMs: 400, Angles: j(0, 0, 0, 0, -15, 0)},
				{AtMs: 600, Angles: j(0, 0, 0, 0, 15, 0)},
				{AtMs: 800, Angles: home},
			},
		},
		// 摇头：头只能俯仰，故用身体偏航表示否定（量程 ±90，摆大才看得出来）。
		{
			Name:    "shake",
			Emotion: "confused",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 250, Angles: j(0, 0, 0, 0, 0, 45)},
				{AtMs: 550, Angles: j(0, 0, 0, 0, 0, -45)},
				{AtMs: 850, Angles: j(0, 0, 0, 0, 0, 45)},
				{AtMs: 1150, Angles: home},
			},
		},
		// 万岁：双臂高举过头 + 仰头欢呼。
		{
			Name:    "cheer",
			Emotion: "happy",
			Loops:   1,
			Frames: []Keyframe{
				{AtMs: 0, Angles: home},
				{AtMs: 500, Angles: j(28, 140, 28, 140, -12, 0)}, // 双臂冲天（横滚张开、俯仰 140，避开头）
				{AtMs: 850, Angles: j(28, 140, 28, 140, 15, 0)},  // 仰头
				{AtMs: 1200, Angles: j(28, 140, 28, 140, -12, 0)},
				{AtMs: 1800, Angles: home},
			},
		},
		// 跳舞：按 danceBPM 踩拍编排的一段 8 拍乐句，自带「表情轨道」（踩拍变脸）。
		//
		// 编排原则（吃过亏才总结出来的）：
		//   1. 抬臂用【大正角度】（0=垂下、150=高举）。老版本写 -60/-70，被 -20 的下限全掐掉，
		//      胳膊几乎不动——这就是"幅度太小、看着不过瘾"的根因。
		//   2. 大幅度的手臂动作只放在【整拍】（120BPM = 500ms/拍），够舵机走完 130° 左右；
		//      半拍只做身体/头的小幅点缀。否则舵机追不上关键帧，动作会糊、反而更像"滞后"。
		//   3. 高低落差要拉满（举到 150 再砸回 15），冲击力全靠这个对比。
		{
			Name:    "dance",
			Emotion: "happy",
			Loops:   4, // 跳 4 个乐句（约 16 秒 @120BPM）；UI 可传入其它循环次数
			Frames: []Keyframe{
				// 拍1：起势，双臂半举外展
				{AtMs: bt(0), Angles: j(20, 70, 20, 70, 6, 0), Emotion: "happy"},
				{AtMs: bt(0.5), Angles: j(20, 70, 20, 70, -10, 20)}, // 半拍：只晃身体和头（律动）
				// 拍2：右臂冲天 + 身体右转
				{AtMs: bt(1), Angles: j(8, 20, 30, 140, 10, 55)},
				{AtMs: bt(1.5), Angles: j(8, 20, 30, 140, -8, 35)},
				// 拍3：左臂冲天 + 身体左转（变脸 → cool 耍帅）
				{AtMs: bt(2), Angles: j(30, 140, 8, 20, 10, -55), Emotion: "cool"},
				{AtMs: bt(2.5), Angles: j(30, 140, 8, 20, -8, -35)},
				// 拍4：双臂收到胸前蓄力（低头），准备高潮
				{AtMs: bt(3), Angles: j(4, 30, 4, 30, -15, 0)},
				{AtMs: bt(3.5), Angles: j(4, 30, 4, 30, -15, 0)},
				// 拍5：万岁高潮，双臂冲天 + 仰头（变脸 → surprised）
				{AtMs: bt(4), Angles: j(30, 140, 30, 140, 15, 0), Emotion: "surprised"},
				{AtMs: bt(4.5), Angles: j(30, 140, 30, 140, 15, 30)}, // 高举摆胯
				{AtMs: bt(5), Angles: j(30, 140, 30, 140, 15, -30)},
				// 拍6：双臂砸下（大落差＝冲击力），低头
				{AtMs: bt(5.5), Angles: j(6, 15, 6, 15, -15, 0)},
				// 拍7：右手高举招两下 + 身体右摆
				{AtMs: bt(6), Angles: j(4, 15, 30, 140, 8, 50), Emotion: "happy"},
				{AtMs: bt(6.5), Angles: j(4, 15, 18, 140, 8, 50)}, // 半拍只收横滚（招手）；不得低于 18，否则举着的手会往脑袋上招
				// 拍8：左手高举招两下 + 身体左摆
				{AtMs: bt(7), Angles: j(30, 140, 4, 15, 8, -50)},
				{AtMs: bt(7.5), Angles: j(18, 140, 4, 15, 8, -50)},
				// 回起势，无缝衔接下一乐句
				{AtMs: bt(8), Angles: j(20, 70, 20, 70, 6, 0)},
			},
		},
	}
}
