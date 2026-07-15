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
	cur       faceParams
	buf       []byte
	lastKey   uint64
	haveLast  bool
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
	tilt       float64   // 头倾(弧度，confused)
	asym       float64   // 左右不对称(confused)
	col        [3]float64 // 眼/嘴主色（会按情绪微调：sad偏蓝/angry偏红）
}

var (
	sdfMint  = [3]float64{190, 245, 232} // 柔和薄荷（眼/嘴主色）
	sdfSadC  = [3]float64{158, 206, 246} // 悲伤淡蓝
	sdfAngC  = [3]float64{246, 188, 194} // 愤怒淡红（可爱不刺眼）
	sdfHi    = [3]float64{244, 255, 253} // 高光白
	sdfBgCtr = [3]float64{40, 58, 60}    // 背景中心（暖一点的深青灰）
	sdfBgEdge = [3]float64{12, 20, 30}   // 背景边缘（更深偏蓝）
)

// emotionTarget 给出某情绪的目标表情参数。
func emotionTarget(e string) faceParams {
	p := faceParams{eyeOpen: 1, eyeScale: 1, mouthCurve: 0.42, col: sdfMint}
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
		p.col = sdfSadC
	case "angry":
		p.lidTop = 0.32
		p.lidAngle = 0.55
		p.mouthCurve = -0.38
		p.eyeScale = 0.98
		p.col = sdfAngC
	case "surprised":
		p.eyeScale = 1.30
		p.mouthOpen = 0.72
		p.mouthCurve = 0.15
	case "confused":
		p.tilt = 0.14
		p.asym = 1
		p.lidTop = 0.12
		p.mouthCurve = -0.08
	}
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
	for i := 0; i < 3; i++ {
		c.col[i] = e(c.col[i], t.col[i])
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
	add(s.cur.gazeX)
	add(s.cur.gazeY)
	add(s.cur.mouthCurve)
	add(s.mouthOpenEff())
	add(s.cur.tilt)
	add(s.cur.asym)
	add(s.cur.col[0])
	add(s.cur.col[1])
	add(s.cur.col[2])
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
	g := strength * math.Exp(-math.Max(d, 0)/radius)
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

// drawHi 画一只眼的双高光（大 + 小），alpha 随睁眼度淡入淡出。
func drawHi(acc *[3]float64, px, py, cx, cy, hw, hh, alpha float64) {
	if alpha <= 0.02 {
		return
	}
	// 大高光（偏上偏内）
	composite(acc, sdfHi, cov(math.Hypot(px-(cx-hw*0.30), py-(cy-hh*0.40))-hw*0.26)*alpha)
	// 小高光（偏下偏外）
	composite(acc, sdfHi, cov(math.Hypot(px-(cx+hw*0.22), py-(cy-hh*0.06))-hw*0.12)*alpha)
}

// render 逐像素渲染当前表情到 buf。
func (s *SDFFaceSource) render() {
	const (
		cx0, cy0 = 120.0, 120.0
		eyeY     = 104.0
		eyeDX    = 44.0
		mouthCy  = 172.0
		scrR     = 119.0
	)
	blink := s.blinkVal()
	eyeOpen := clampf(s.cur.eyeOpen*(1-blink), 0, 1)
	mo := s.mouthOpenEff()
	col := s.cur.col
	sq, lt, la := s.cur.squint, s.cur.lidTop, s.cur.lidAngle
	tilt := s.cur.tilt
	ct, stt := math.Cos(tilt), math.Sin(tilt)

	hw := 26.0 * s.cur.eyeScale
	hhFull := 31.0 * s.cur.eyeScale
	hh := math.Max(3, hhFull*eyeOpen)
	rC := math.Min(hw, hh) * 0.88
	rScale := 1 - 0.14*s.cur.asym // confused 右眼略小
	lcx, rcx := cx0-eyeDX, cx0+eyeDX
	hiA := clampf(eyeOpen*(1-sq*1.3), 0, 1) // 眯眼/闭眼时收起高光

	// 嘴：抛物线折线 + 张嘴椭圆。
	const mn = 15
	var mpx, mpy [mn]float64
	for i := 0; i < mn; i++ {
		u := float64(i)/(mn-1)*2 - 1
		mpx[i] = cx0 + u*24
		mpy[i] = mouthCy + s.cur.mouthCurve*15*(1-u*u)
	}
	openRy := mo * 22

	for y := 0; y < scrH; y++ {
		fy := float64(y) - cy0
		for x := 0; x < scrW; x++ {
			fx := float64(x) - cx0
			px := cx0 + fx*ct - fy*stt // 头倾：反旋转到脸坐标
			py := cy0 + fx*stt + fy*ct

			// 背景：深青灰径向渐变（中心亮、边缘深）。
			rr := math.Hypot(fx, fy)
			bt := clampf(rr/135, 0, 1)
			acc := [3]float64{
				lerpf(sdfBgCtr[0], sdfBgEdge[0], bt),
				lerpf(sdfBgCtr[1], sdfBgEdge[1], bt),
				lerpf(sdfBgCtr[2], sdfBgEdge[2], bt),
			}

			// 眼 SDF。
			dL := eyeSDF(px, py, lcx, eyeY, hw, hh, rC, sq, lt, la, +1)
			dR := eyeSDF(px, py, rcx, eyeY, hw*rScale, hh*rScale, rC*rScale, sq, lt, la, -1)

			// 嘴 SDF（仅嘴带内算；带要够宽以覆盖辉光衰减，否则边界会有一道缝）。
			dM := 1e9
			if py > 126 && py < 218 {
				for i := 0; i < mn-1; i++ {
					if dd := sdfSeg(px, py, mpx[i], mpy[i], mpx[i+1], mpy[i+1]); dd < dM {
						dM = dd
					}
				}
				dM -= 4.5 // 嘴较粗、圆润
				if openRy > 0.5 {
					if do := sdfEllipse(px, py, cx0, mouthCy, 20, openRy); do < dM {
						dM = do
					}
				}
			}

			// 辉光（在填充之下，向外扩散形成 bloom）。
			addGlow(&acc, col, dL, 16, 0.95)
			addGlow(&acc, col, dR, 16, 0.95)
			if dM < 1e8 {
				addGlow(&acc, col, dM, 12, 0.75)
			}
			// 实体填充。
			composite(&acc, col, cov(dL))
			composite(&acc, col, cov(dR))
			if dM < 1e8 {
				composite(&acc, col, cov(dM))
			}
			// 双高光。
			drawHi(&acc, px, py, lcx, eyeY, hw, hh, hiA)
			drawHi(&acc, px, py, rcx, eyeY, hw*rScale, hh*rScale, hiA)

			// 圆屏遮罩：边缘淡出到黑（设备是圆屏）。
			m := clampf(scrR-rr, 0, 1)
			i := (y*scrW + x) * 3
			s.buf[i] = byte(clampf(acc[0]*m, 0, 255))
			s.buf[i+1] = byte(clampf(acc[1]*m, 0, 255))
			s.buf[i+2] = byte(clampf(acc[2]*m, 0, 255))
		}
	}
}
