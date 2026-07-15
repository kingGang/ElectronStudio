package display

import (
	"math"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// SDFFaceSource 是基于【有向距离场(SDF)】的实时表情脸：每帧逐像素求五官的 SDF、抗锯齿合成，
// 与片元着色器(fragment shader)是同一套数学，只是在 CPU 上算（本工程 CGO_ENABLED=0、要交叉
// 编到树莓派，用不了 GPU）。240×240 逐像素在桌面/树莓派上跑 30fps 都很轻松。
//
// 风格走"可爱发光机器人眼"：柔和薄荷色的大圆眼带辉光(bloom)与双高光、轻柔的微笑弧、深青灰渐变
// 背景。表情靠眼形/眼睑/嘴角连续 morph，不用生硬的眉毛。
//
// 与 EmotionSource 同实现 Face（+Invalidate），由 config.face = "sdf" 选用。说话口型可由
// SetSpeaking 的节奏兜底，若上层喂入 SetMouthLevel（如实时下行音频 RMS）则按真实音量张合、贴合对话。
type SDFFaceSource struct {
	mu        sync.Mutex
	emotion   string
	speaking  bool
	level     float64 // 外部喂入的口型音量 0..1（实时下行 RMS）；未喂时用节奏兜底
	levelTick int
	tick      int
	nextBlink int
	blinkT    int
	blinkDur  int
	// idle 扫视（眼珠自主看看别处，显得灵动）：当前/目标偏移 + 下次扫视时刻 + 伪随机种子。
	sacX, sacY, sacTX, sacTY float64
	nextSac                  int
	sacSeed                  uint32
	cur                      faceParams
	buf                      []byte
	lastKey                  uint64
	haveLast                 bool
}

// gazeTargets 是 idle 扫视的候选落点（含多次回中，避免一直乱瞟）。
var gazeTargets = [][2]float64{
	{0, 0}, {0, 0}, {0, 0},
	{-0.65, -0.08}, {0.65, -0.08},
	{-0.45, 0.28}, {0.5, 0.24},
	{0, -0.3}, {0.25, 0.05}, {-0.28, 0.05},
}

// faceParams 是一组连续表情参数；每种情绪一组目标值，当前值每帧向目标缓动。
type faceParams struct {
	eyeOpen   float64    // 0..1 睁眼度（眨眼→0）
	squint    float64    // 0..1 下眼睑上抬，开心眯眼 ∩
	lidTop    float64    // 0..1 上眼睑下压（变窄，愤怒/困）
	lidAngle  float64    // 上眼睑倾斜：+ 内端低(愤怒) / - 外端低(悲伤)
	eyeScale  float64    // 眼整体大小（surprised 更大）
	gazeX     float64    // 视线水平 -1..1
	gazeY     float64    // 视线垂直 -1..1
	mouthCurve float64   // 嘴角 -1皱..+1笑（neutral 也带点微笑更可爱）
	mouthOpen  float64   // 0..1 张嘴基线（surprised）
	tilt       float64    // 头倾(弧度，confused)
	asym       float64    // 左右不对称(confused)
	cross      float64    // 斗鸡眼：两眼向内看（鬼脸）
	tongue     float64    // 吐舌 0..1（鬼脸）
	colA       [3]float64 // 眼睛【上部】渐变色（顶色，通常更亮更青）
	colB       [3]float64 // 眼睛【下部】渐变色（底色，通常更深偏蓝）——上下双色让颜色更丰富、有质感
}

var (
	sdfHi     = [3]float64{236, 255, 252} // 高光白
	sdfPupil  = [3]float64{20, 96, 150}   // 眼珠/虹膜（较深的蓝，配大高光才像参考图；实机上眼够大就不会掏成洞）
	sdfTongue = [3]float64{255, 120, 150} // 吐舌（暖粉，鬼脸用）
)

// 情绪→眼睛上下渐变色对（top,bottom）。刻意用高纯度色：实机自发光屏 + 辉光会把浅色冲白，
// 饱和的渐变才通透、有"宝石感"，颜色也更丰富。
func emotionColors(e string) (top, bot [3]float64) {
	// 都刻意【高亮高纯度】：G/B 拉满出亮度，但把"离得远的那个通道"(青蓝的 R / 暖色的 B)压低，
	// 这样够鲜亮又不会在实机上冲成白。
	switch e {
	case "happy":
		return [3]float64{130, 255, 170}, [3]float64{20, 235, 255} // 亮绿 → 亮青
	case "sad":
		return [3]float64{80, 190, 255}, [3]float64{80, 90, 255} // 亮蓝 → 亮靛
	case "angry":
		return [3]float64{255, 160, 60}, [3]float64{255, 40, 110} // 亮橙 → 亮红品
	case "surprised":
		return [3]float64{90, 255, 245}, [3]float64{40, 190, 255} // 亮青 → 亮蓝
	case "confused":
		return [3]float64{140, 255, 200}, [3]float64{70, 230, 240} // 亮薄荷 → 亮青
	case "silly":
		return [3]float64{150, 255, 140}, [3]float64{70, 250, 130} // 俏皮亮绿
	default: // neutral
		return [3]float64{80, 255, 230}, [3]float64{20, 190, 255} // 亮青绿 → 亮蓝
	}
}

// emotionTarget 给出某情绪的目标表情参数。
func emotionTarget(e string) faceParams {
	p := faceParams{eyeOpen: 1, eyeScale: 1, mouthCurve: 0.42}
	switch e {
	case "happy":
		p.squint = 0.55
		p.mouthCurve = 0.98
		p.eyeScale = 1.03
	case "sad":
		p.lidAngle = -0.5
		p.gazeY = 0.32
		p.mouthCurve = -0.55
		p.eyeOpen = 0.92
	case "angry":
		p.lidTop = 0.32
		p.lidAngle = 0.55
		p.mouthCurve = -0.38
		p.eyeScale = 0.98
	case "surprised":
		p.eyeScale = 1.30
		p.mouthOpen = 0.72
		p.mouthCurve = 0.15
	case "confused":
		p.tilt = 0.14
		p.asym = 1
		p.lidTop = 0.12
		p.mouthCurve = -0.08
	case "silly": // 鬼脸：斗鸡眼 + 咧嘴傻笑（同色系，不用突兀的粉舌头）
		p.cross = 0.9
		p.mouthOpen = 0.42
		p.mouthCurve = 0.9
	}
	p.colA, p.colB = emotionColors(e)
	return p
}

// NewSDFFaceSource 创建 SDF 表情脸，初始 neutral。
func NewSDFFaceSource() *SDFFaceSource {
	return &SDFFaceSource{
		emotion:   "neutral",
		cur:       emotionTarget("neutral"),
		buf:       make([]byte, robot.ImageBytesRGB888),
		nextBlink: 55,
		blinkDur:  7,
		nextSac:   40,
		sacSeed:   0x2545F491,
	}
}

// SetEmotion 设定情绪（实现 Face）。
func (s *SDFFaceSource) SetEmotion(e string) {
	s.mu.Lock()
	s.emotion = e
	s.mu.Unlock()
}

// SetSpeaking 设定是否说话（实现 Face）。
func (s *SDFFaceSource) SetSpeaking(b bool) {
	s.mu.Lock()
	s.speaking = b
	if !b {
		s.level = 0
	}
	s.mu.Unlock()
}

// SetMouthLevel 喂入实时口型音量(0..1)，让嘴按真实说话音量张合、与对话同步。
func (s *SDFFaceSource) SetMouthLevel(l float64) {
	s.mu.Lock()
	s.level = clampf(l, 0, 1)
	s.levelTick = s.tick
	s.mu.Unlock()
}

// Invalidate 让下一帧必定重渲染。
func (s *SDFFaceSource) Invalidate() {
	s.mu.Lock()
	s.haveLast = false
	s.mu.Unlock()
}

// Frame 实现 Source：推进一帧；仅当画面实际变化时返回新帧，否则 nil（省带宽、护 USB）。
func (s *SDFFaceSource) Frame() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick++
	s.step()
	key := s.visualKey()
	if s.haveLast && key == s.lastKey {
		return nil
	}
	s.lastKey = key
	s.haveLast = true
	s.render()
	out := make([]byte, len(s.buf))
	copy(out, s.buf)
	return out
}

// step 缓动当前参数→目标情绪，并推进眨眼与口型音量衰减。
func (s *SDFFaceSource) step() {
	t := emotionTarget(s.emotion)
	const k = 0.22
	e := func(cur, tgt float64) float64 {
		n := cur + (tgt-cur)*k
		if math.Abs(tgt-n) < 0.004 {
			return tgt
		}
		return n
	}
	c := &s.cur
	c.eyeOpen = e(c.eyeOpen, t.eyeOpen)
	c.squint = e(c.squint, t.squint)
	c.lidTop = e(c.lidTop, t.lidTop)
	c.lidAngle = e(c.lidAngle, t.lidAngle)
	c.eyeScale = e(c.eyeScale, t.eyeScale)
	c.gazeX = e(c.gazeX, t.gazeX)
	c.gazeY = e(c.gazeY, t.gazeY)
	c.mouthCurve = e(c.mouthCurve, t.mouthCurve)
	c.mouthOpen = e(c.mouthOpen, t.mouthOpen)
	c.tilt = e(c.tilt, t.tilt)
	c.asym = e(c.asym, t.asym)
	c.cross = e(c.cross, t.cross)
	c.tongue = e(c.tongue, t.tongue)
	for i := 0; i < 3; i++ {
		c.colA[i] = e(c.colA[i], t.colA[i])
		c.colB[i] = e(c.colB[i], t.colB[i])
	}

	if s.blinkT > 0 {
		s.blinkT++
		if s.blinkT > s.blinkDur {
			s.blinkT = 0
			s.nextBlink = s.tick + 65 + (s.tick*13)%40
		}
	} else if s.tick >= s.nextBlink {
		s.blinkT = 1
	}

	// idle 扫视：到点挑个新落点，眼珠快速移过去、停留一会儿。让脸显得在"看东西"、更灵动。
	if s.tick >= s.nextSac {
		s.sacSeed = s.sacSeed*1103515245 + 12345
		tg := gazeTargets[int(s.sacSeed%uint32(len(gazeTargets)))]
		s.sacTX, s.sacTY = tg[0], tg[1]
		s.nextSac = s.tick + 28 + int((s.sacSeed>>16)%46) // 停留 ~0.9~2.5s，看得更勤、更活
	}
	se := func(cur, tgt float64) float64 {
		n := cur + (tgt-cur)*0.45 // 扫视要快（saccade）
		if math.Abs(tgt-n) < 0.004 {
			return tgt
		}
		return n
	}
	tx, ty := s.sacTX, s.sacTY
	if s.speaking { // 说话时收敛视线、看着对方，显得专注（说话态基础表情）
		tx, ty = tx*0.3, ty*0.3
	}
	s.sacX = se(s.sacX, tx)
	s.sacY = se(s.sacY, ty)
	if s.tick-s.levelTick > 5 && s.level > 0 {
		s.level *= 0.7
		if s.level < 0.02 {
			s.level = 0
		}
	}
}

func (s *SDFFaceSource) blinkVal() float64 {
	if s.blinkT <= 0 {
		return 0
	}
	return math.Sin(math.Pi * float64(s.blinkT) / float64(s.blinkDur+1))
}

// breath 返回辉光"呼吸"系数（~4.6s 周期，±22%）：光晕柔和涨落。幅度不宜大，否则低色深屏会闪。
func (s *SDFFaceSource) breath() float64 {
	return 1 + 0.22*math.Sin(float64(s.tick)*0.046)
}

// floatY 是整张脸缓慢上下"浮动"的位移（~5s 周期，±2.4px）：移动的是实心形状、不像渐变那样在
// 低色深屏上带状化，读起来像轻轻呼吸/漂浮，很自然的灵动感。
func (s *SDFFaceSource) floatY() float64 {
	return 2.4 * math.Sin(float64(s.tick)*0.041)
}

func (s *SDFFaceSource) mouthOpenEff() float64 {
	mo := s.cur.mouthOpen
	if s.speaking || s.level > 0.02 {
		sp := 0.12 + 0.30*(0.5+0.5*math.Sin(float64(s.tick)*0.9))
		if s.level > 0.02 {
			sp = clampf(s.level*1.4, 0, 1)
		}
		if sp > mo {
			mo = sp
		}
	}
	return mo
}

func (s *SDFFaceSource) visualKey() uint64 {
	h := uint64(1469598103934665603)
	add := func(f float64) {
		q := uint64(int64(math.Round(f*48)) + (1 << 24))
		h = (h ^ q) * 1099511628211
	}
	add(s.cur.eyeOpen * (1 - s.blinkVal()))
	add(s.cur.squint)
	add(s.cur.lidTop)
	add(s.cur.lidAngle)
	add(s.cur.eyeScale)
	add(s.cur.gazeX + s.sacX) // 含 idle 扫视，眼珠移动时才推帧
	add(s.cur.gazeY + s.sacY)
	add(s.cur.mouthCurve)
	add(s.mouthOpenEff())
	add(s.cur.tilt)
	add(s.cur.asym)
	add(s.cur.cross)
	add(s.cur.tongue)
	for i := 0; i < 3; i++ {
		add(s.cur.colA[i])
		add(s.cur.colB[i])
	}
	add(s.breath())      // 呼吸脉动：光晕涨落时也要推帧
	add(s.floatY() * 4)  // 整脸浮动：位移变化也要推帧（放大量化精度，让浮动更连续）
	return h
}

// ---- SDF 基元与工具 ----

// sdfRoundBox 圆角矩形 SDF（负=内部）。r 越大越接近圆润的"软眼"。
func sdfRoundBox(px, py, cx, cy, hw, hh, r float64) float64 {
	qx := math.Abs(px-cx) - (hw - r)
	qy := math.Abs(py-cy) - (hh - r)
	ax, ay := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ax, ay) + math.Min(math.Max(qx, qy), 0) - r
}

func sdfEllipse(px, py, cx, cy, rx, ry float64) float64 {
	dx, dy := (px-cx)/rx, (py-cy)/ry
	k := math.Hypot(dx, dy)
	if k == 0 {
		return -math.Min(rx, ry)
	}
	return (k - 1) * math.Min(rx, ry)
}

func sdfSeg(px, py, ax, ay, bx, by float64) float64 {
	pax, pay := px-ax, py-ay
	bax, bay := bx-ax, by-ay
	d := bax*bax + bay*bay
	h := 0.0
	if d > 0 {
		h = clampf((pax*bax+pay*bay)/d, 0, 1)
	}
	return math.Hypot(pax-bax*h, pay-bay*h)
}

func clampf(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
func lerpf(a, b, t float64) float64 { return a + (b-a)*t }

// cov 由有向距离得覆盖率(抗锯齿)：约 1.2px 过渡带，稍软。
func cov(d float64) float64 { return clampf(0.5-d/1.2, 0, 1) }

// addGlow 把发光色按到形状边缘的距离做指数衰减叠加（screen 混合，形成柔和 bloom，不易过曝）。
func addGlow(acc *[3]float64, col [3]float64, d, radius, strength float64) {
	if d > radius*3.5 {
		return
	}
	g := clampf(strength*math.Exp(-math.Max(d, 0)/radius), 0, 1) // 呼吸时 strength 可能 >1，夹住免过曝
	if g <= 0.002 {
		return
	}
	for i := 0; i < 3; i++ {
		acc[i] = 255 - (255-acc[i])*(1-g*col[i]/255)
	}
}

// composite 用覆盖率把 col 叠到 acc（over 混合）。
func composite(acc *[3]float64, col [3]float64, c float64) {
	if c <= 0 {
		return
	}
	acc[0] = lerpf(acc[0], col[0], c)
	acc[1] = lerpf(acc[1], col[1], c)
	acc[2] = lerpf(acc[2], col[2], c)
}

// mix3 在两颜色间线性插值（用于眼睛上下双色渐变）。
func mix3(a, b [3]float64, t float64) [3]float64 {
	return [3]float64{lerpf(a[0], b[0], t), lerpf(a[1], b[1], t), lerpf(a[2], b[2], t)}
}

// eyeGrad 眼睛某像素的填充色：上(top)→下(bot)双色渐变，叠加顶部提亮的 gloss 光泽，像发光宝石。
func eyeGrad(top, bot [3]float64, py, cy, hh float64) [3]float64 {
	t := clampf((py-(cy-hh))/(2*hh), 0, 1)
	c := mix3(top, bot, t)
	gloss := 1.18 - 0.12*t // 顶更亮、底也不压太暗（整体更亮丽）
	return [3]float64{clampf(c[0]*gloss, 0, 255), clampf(c[1]*gloss, 0, 255), clampf(c[2]*gloss, 0, 255)}
}

// eyeSDF 计算一只眼的距离：圆角矩形 + 开心下切(∩) + 上眼睑下压/倾斜。sign 区分左右(倾斜镜像)。
func eyeSDF(px, py, cx, cy, hw, hh, rC float64, squint, lidTop, lidAngle, sign float64) float64 {
	d := sdfRoundBox(px, py, cx, cy, hw, hh, rC)
	if squint > 0.02 { // 开心：下方挖上升的圆 → ∩ 眯眼
		carveCy := cy + hh*(1.25-0.95*squint)
		dc := math.Hypot(px-cx, py-carveCy) - hh*1.32
		d = math.Max(d, -dc)
	}
	if lidTop > 0.02 || math.Abs(lidAngle) > 0.02 { // 上眼睑：下压 + 倾斜，盖住上半
		lidY := cy - hh + lidTop*2*hh
		lineY := lidY + (lidAngle*sign)*(px-cx)
		d = math.Max(d, -(py - lineY)) // py<lineY(在盖线之上)→移除
	}
	return d
}

// drawEyeball 画一只眼的【眼珠】：深青圆珠随视线(gx,gy)在眼内移动 + 双高光，整体裁进眼形内
// （dEye 为该像素到眼形的距离，只在眼内绘制，免得极端视线时眼珠跑到眼外的黑底上）。
func drawEyeball(acc *[3]float64, px, py, cx, cy, hw, hh, gx, gy, alpha, dEye float64) {
	a := alpha * cov(dEye) // 只在眼内
	if a <= 0.02 {
		return
	}
	pcx := cx + gx*hw*0.4 // 眼珠随视线移动
	pcy := cy + gy*hh*0.34
	pr := hw * 0.4 // 眼珠小一点，别把眼睛掏成一个大黑洞（实机上会碎成横条）
	composite(acc, sdfPupil, cov(sdfEllipse(px, py, pcx, pcy, pr, pr*1.05))*a) // 眼珠
	// 双高光（眼珠上偏上，跟着眼珠一起动）
	composite(acc, sdfHi, cov(math.Hypot(px-(pcx-pr*0.32), py-(pcy-pr*0.42))-pr*0.28)*a)
	composite(acc, sdfHi, cov(math.Hypot(px-(pcx+pr*0.28), py-(pcy-pr*0.04))-pr*0.14)*a)
}

// render 逐像素渲染当前表情到 buf。
func (s *SDFFaceSource) render() {
	const (
		cx0, cy0 = 120.0, 120.0
		eyeDX    = 44.0
		scrR     = 119.0
	)
	fl := s.floatY()      // 整脸缓慢上下浮动
	eyeY := 104.0 + fl    // 眼、嘴一起浮，像轻轻呼吸
	mouthCy := 172.0 + fl
	blink := s.blinkVal()
	eyeOpen := clampf(s.cur.eyeOpen*(1-blink), 0, 1)
	mo := s.mouthOpenEff()
	colA, colB := s.cur.colA, s.cur.colB // 眼睛上下渐变色
	glowCol := colA                      // 辉光用亮顶色，光晕更通透
	br := s.breath()                     // 呼吸脉动系数
	sq, lt, la := s.cur.squint, s.cur.lidTop, s.cur.lidAngle
	tilt := s.cur.tilt
	ct, stt := math.Cos(tilt), math.Sin(tilt)

	hw := 32.0 * s.cur.eyeScale // 眼睛更大更饱满（贴近参考图的大圆眼）
	hhFull := 40.0 * s.cur.eyeScale
	hh := math.Max(3, hhFull*eyeOpen)
	rC := math.Min(hw, hh) * 0.88
	rScale := 1 - 0.14*s.cur.asym // confused 右眼略小
	lcx, rcx := cx0-eyeDX, cx0+eyeDX
	hiA := clampf(eyeOpen*(1-sq*1.3), 0, 1)              // 眯眼/闭眼时收起眼珠
	gx := clampf(s.cur.gazeX+s.sacX, -1, 1)             // 有效视线 = 情绪偏置 + idle 扫视
	gy := clampf(s.cur.gazeY+s.sacY, -1, 1)

	// 嘴：小而干净的微笑弧 + 张嘴椭圆（对齐弧心，避免错位导致"嘴巴很奇怪"）。
	const mn = 15
	mCurveY := mouthCy + s.cur.mouthCurve*12 // 微笑弧的中心 y
	var mpx, mpy [mn]float64
	for i := 0; i < mn; i++ {
		u := float64(i)/(mn-1)*2 - 1
		mpx[i] = cx0 + u*18 // 嘴更窄、更小巧
		mpy[i] = mouthCy + s.cur.mouthCurve*12*(1-u*u)
	}
	openRy := mo * 15
	openCy := mCurveY + openRy*0.4 // 张嘴向下开，跟弧心对齐
	cross := s.cur.cross           // 斗鸡眼：两眼向内看（鬼脸）

	for y := 0; y < scrH; y++ {
		fy := float64(y) - cy0
		for x := 0; x < scrW; x++ {
			fx := float64(x) - cx0
			px := cx0 + fx*ct - fy*stt // 头倾：反旋转到脸坐标
			py := cy0 + fx*stt + fy*ct

			// 背景【纯黑】：设备是自发光 LCD，只有黑=像素灭，发光的眼/嘴才有对比度、才亮眼。
			// （渐变背景会点亮整屏、把辉光糊成一片低对比的蓝雾——实机实测很糟。）
			rr := math.Hypot(fx, fy)
			acc := [3]float64{0, 0, 0}

			// 眼 SDF。
			dL := eyeSDF(px, py, lcx, eyeY, hw, hh, rC, sq, lt, la, +1)
			dR := eyeSDF(px, py, rcx, eyeY, hw*rScale, hh*rScale, rC*rScale, sq, lt, la, -1)

			// 嘴 SDF（仅嘴带内算；带要够宽以覆盖辉光衰减，否则边界会有一道缝）。
			dM := 1e9
			if py > 130 && py < 214 {
				for i := 0; i < mn-1; i++ {
					if dd := sdfSeg(px, py, mpx[i], mpy[i], mpx[i+1], mpy[i+1]); dd < dM {
						dM = dd
					}
				}
				dM -= 3.4 // 嘴线细一点、干净小巧
				if openRy > 0.5 {
					if do := sdfEllipse(px, py, cx0, openCy, 12, openRy); do < dM {
						dM = do
					}
				}
			}

			// 辉光（填充之下向外扩散成 bloom）。收紧半径，别让左右眼的光在中间糊成一坨、
			// 也别铺满整屏——实机小屏上要"发光的眼"而不是"一片光雾"。
			addGlow(&acc, glowCol, dL, 8.5, 0.9*br)
			addGlow(&acc, glowCol, dR, 8.5, 0.9*br)
			if dM < 1e8 {
				addGlow(&acc, glowCol, dM, 7, 0.72*br)
			}
			// 实体填充：眼睛上下双色渐变 + 顶部提亮(gloss)，颜色更丰富、有宝石质感；嘴用亮顶色。
			if cL := cov(dL); cL > 0 {
				composite(&acc, eyeGrad(colA, colB, py, eyeY, hh), cL)
			}
			if cR := cov(dR); cR > 0 {
				composite(&acc, eyeGrad(colA, colB, py, eyeY, hh*rScale), cR)
			}
			if dM < 1e8 {
				composite(&acc, colA, cov(dM))
			}
			// 眼珠（随视线移动）+ 高光。
			drawEyeball(&acc, px, py, lcx, eyeY, hw, hh, clampf(gx+cross, -1.2, 1.2), gy, hiA, dL)
			drawEyeball(&acc, px, py, rcx, eyeY, hw*rScale, hh*rScale, clampf(gx-cross, -1.2, 1.2), gy, hiA, dR)
				// 吐舌（鬼脸）：嘴下方一枚暖色小圆舌。
				if s.cur.tongue > 0.05 && py > mCurveY {
					dt := sdfEllipse(px, py, cx0, mCurveY+6+s.cur.tongue*5, 8, 11*s.cur.tongue)
					composite(&acc, sdfTongue, cov(dt)*clampf(s.cur.tongue*1.5, 0, 1))
				}

			// 圆屏遮罩：边缘淡出到黑（设备是圆屏）。
			m := clampf(scrR-rr, 0, 1)
			i := (y*scrW + x) * 3
			s.buf[i] = byte(clampf(acc[0]*m, 0, 255))
			s.buf[i+1] = byte(clampf(acc[1]*m, 0, 255))
			s.buf[i+2] = byte(clampf(acc[2]*m, 0, 255))
		}
	}
}
