package display

import (
	"math"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// SDFFaceSource 是基于【有向距离场(SDF)】的实时表情脸：每帧逐像素求五官的 SDF、抗锯齿合成，
// 与片元着色器(fragment shader)同一套数学，只是在 CPU 上算（本工程 CGO_ENABLED=0、要交叉编到
// 树莓派，用不了 GPU）。240×240 逐像素跑 30fps 很轻松。
//
// 视觉照用户给的参考图：纯黑底 + 发光的【实心圆角矩形大眼】(每只眼两颗白高光、无深色瞳孔) +
// 情绪各自的实心色(青/蓝/橙/黄/紫/绿) + 需要时加【深色斜眉条】(难过/生气/疑惑)，嘴是简洁的
// 短横/弧线。眨眼、辉光呼吸、整脸浮动、眼睛微微看向别处带来生命感。
type SDFFaceSource struct {
	mu        sync.Mutex
	emotion   string
	speaking  bool
	level     float64
	levelTick int
	tick      int
	nextBlink int
	blinkT    int
	blinkDur  int
	sacX, sacY, sacTX, sacTY float64
	nextSac                  int
	sacSeed                  uint32
	cur                      faceParams
	buf                      []byte
	lastKey                  uint64
	haveLast                 bool
}

// gazeTargets 是 idle 扫视的候选落点（含多次回中）。
var gazeTargets = [][2]float64{
	{0, 0}, {0, 0}, {0, 0},
	{-0.6, -0.05}, {0.6, -0.05},
	{-0.4, 0.22}, {0.45, 0.2},
	{0, -0.22}, {0.22, 0.05}, {-0.24, 0.05},
}

// faceParams 是连续表情参数；每种情绪一组目标值，当前值每帧向目标缓动。
type faceParams struct {
	eyeOpen   float64 // 0..1 睁眼度（眨眼→0）
	eyeScale  float64 // 眼整体大小
	tall      float64 // 眼额外拉高（surprised 的竖长眼）
	squint    float64 // happy 下眼睑上抬一点
	lidTop    float64 // 上眼睑下压（半眯，unamused）
	gazeX     float64 // 视线水平 -1..1
	gazeY     float64 // 视线垂直 -1..1
	brow      float64 // 眉条显示程度 0..1（0=不画眉）
	browAngle float64 // 眉内端：+ 上挑(难过/无辜) / - 下压(生气)
	browRaise float64 // 眉整体抬高（scared 更高、angry 更低）
	cross     float64 // 斗鸡眼（鬼脸）
	asym      float64 // 左右不对称（疑惑）
	mouthCurve float64 // 嘴角 -1皱..+1笑（0=平直短横）
	mouthOpen  float64 // 0..1 张嘴基线
	tilt       float64 // 头倾（疑惑）
	col        [3]float64 // 眼/嘴主色（实心）
}

var (
	sdfHi   = [3]float64{240, 255, 254} // 高光白
	sdfBrow = [3]float64{40, 48, 66}    // 眉条深色（盖住眼上部的辉光，形成斜眉）
)

// emotionColor 给出情绪的实心主色（照参考图）。
func emotionColor(e string) [3]float64 {
	switch e {
	case "sad":
		return [3]float64{150, 200, 247} // 淡蓝
	case "angry":
		return [3]float64{247, 165, 130} // 橙桃
	case "surprised":
		return [3]float64{245, 236, 156} // 亮黄
	case "confused":
		return [3]float64{212, 180, 247} // 淡紫
	case "silly":
		return [3]float64{150, 246, 150} // 亮绿
	default: // neutral / happy
		return [3]float64{124, 240, 240} // 青
	}
}

// emotionTarget 给出某情绪的目标表情参数（照参考图）。
func emotionTarget(e string) faceParams {
	p := faceParams{eyeOpen: 1, eyeScale: 1}
	switch e {
	case "happy":
		p.mouthCurve = 0.75
		p.squint = 0.12
	case "sad":
		p.mouthCurve = -0.5
		p.brow = 1
		p.browAngle = 0.6 // 内端上挑
		p.browRaise = 0.15
		p.gazeY = 0.16
		p.eyeOpen = 0.9
	case "angry":
		p.mouthCurve = -0.42
		p.brow = 1
		p.browAngle = -0.7 // 内端下压
		p.browRaise = -0.25
	case "surprised":
		p.tall = 0.18
		p.mouthOpen = 0.5
	case "confused":
		p.brow = 1
		p.browAngle = 0.35
		p.browRaise = 0.5
		p.tilt = 0.14
		p.asym = 1
		p.mouthCurve = -0.15
	case "silly":
		p.cross = 0.9
		p.mouthCurve = 0.9
		p.mouthOpen = 0.42
	}
	p.col = emotionColor(e)
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

func (s *SDFFaceSource) SetEmotion(e string) {
	s.mu.Lock()
	s.emotion = e
	s.mu.Unlock()
}

func (s *SDFFaceSource) SetSpeaking(b bool) {
	s.mu.Lock()
	s.speaking = b
	if !b {
		s.level = 0
	}
	s.mu.Unlock()
}

func (s *SDFFaceSource) SetMouthLevel(l float64) {
	s.mu.Lock()
	s.level = clampf(l, 0, 1)
	s.levelTick = s.tick
	s.mu.Unlock()
}

func (s *SDFFaceSource) Invalidate() {
	s.mu.Lock()
	s.haveLast = false
	s.mu.Unlock()
}

// Frame 实现 Source：推进一帧；仅当画面变化时返回新帧，否则 nil（省带宽、护 USB）。
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

// step 缓动参数→目标情绪，并推进眨眼、扫视、口型音量衰减。
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
	c.eyeScale = e(c.eyeScale, t.eyeScale)
	c.tall = e(c.tall, t.tall)
	c.squint = e(c.squint, t.squint)
	c.lidTop = e(c.lidTop, t.lidTop)
	c.gazeX = e(c.gazeX, t.gazeX)
	c.gazeY = e(c.gazeY, t.gazeY)
	c.brow = e(c.brow, t.brow)
	c.browAngle = e(c.browAngle, t.browAngle)
	c.browRaise = e(c.browRaise, t.browRaise)
	c.cross = e(c.cross, t.cross)
	c.asym = e(c.asym, t.asym)
	c.mouthCurve = e(c.mouthCurve, t.mouthCurve)
	c.mouthOpen = e(c.mouthOpen, t.mouthOpen)
	c.tilt = e(c.tilt, t.tilt)
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

	if s.tick >= s.nextSac {
		s.sacSeed = s.sacSeed*1103515245 + 12345
		tg := gazeTargets[int(s.sacSeed%uint32(len(gazeTargets)))]
		s.sacTX, s.sacTY = tg[0], tg[1]
		s.nextSac = s.tick + 32 + int((s.sacSeed>>16)%50)
	}
	se := func(cur, tgt float64) float64 {
		n := cur + (tgt-cur)*0.4
		if math.Abs(tgt-n) < 0.004 {
			return tgt
		}
		return n
	}
	tx, ty := s.sacTX, s.sacTY
	if s.speaking { // 说话时收敛视线、看着对方（专注）
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

// breath 辉光呼吸系数（~4.6s，±20%）。
func (s *SDFFaceSource) breath() float64 { return 1 + 0.2*math.Sin(float64(s.tick)*0.046) }

// floatY 整脸缓慢上下浮动（±2px），移动实心形状、不带状化，读起来像轻轻呼吸。
func (s *SDFFaceSource) floatY() float64 { return 2.0 * math.Sin(float64(s.tick)*0.041) }

// mouthOpenEff 说话时用节奏张合（覆盖整段播放），否则用情绪基线。
func (s *SDFFaceSource) mouthOpenEff() float64 {
	mo := s.cur.mouthOpen
	if s.speaking || s.level > 0.02 {
		sp := 0.14 + 0.32*(0.5+0.5*math.Sin(float64(s.tick)*0.9))
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
	add(s.cur.eyeScale)
	add(s.cur.tall)
	add(s.cur.squint)
	add(s.cur.lidTop)
	add(s.cur.gazeX + s.sacX)
	add(s.cur.gazeY + s.sacY)
	add(s.cur.brow)
	add(s.cur.browAngle)
	add(s.cur.browRaise)
	add(s.cur.cross)
	add(s.cur.asym)
	add(s.cur.mouthCurve)
	add(s.mouthOpenEff())
	add(s.cur.tilt)
	for i := 0; i < 3; i++ {
		add(s.cur.col[i])
	}
	add(s.breath())
	add(s.floatY() * 4)
	return h
}

// ---- SDF 基元与工具 ----

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

func cov(d float64) float64 { return clampf(0.5-d/1.2, 0, 1) }

func addGlow(acc *[3]float64, col [3]float64, d, radius, strength float64) {
	if d > radius*3.5 {
		return
	}
	g := clampf(strength*math.Exp(-math.Max(d, 0)/radius), 0, 1)
	if g <= 0.002 {
		return
	}
	for i := 0; i < 3; i++ {
		acc[i] = 255 - (255-acc[i])*(1-g*col[i]/255)
	}
}

func composite(acc *[3]float64, col [3]float64, c float64) {
	if c <= 0 {
		return
	}
	acc[0] = lerpf(acc[0], col[0], c)
	acc[1] = lerpf(acc[1], col[1], c)
	acc[2] = lerpf(acc[2], col[2], c)
}

// drawHighlights 一只眼的两颗白高光（大：偏上偏内；小：在大高光右下）。gx/gy 让高光随眼微动。
func drawHighlights(acc *[3]float64, px, py, cx, cy, hw, hh, alpha float64) {
	if alpha <= 0.02 {
		return
	}
	composite(acc, sdfHi, cov(math.Hypot(px-(cx-hw*0.30), py-(cy-hh*0.30))-hw*0.27)*alpha)
	composite(acc, sdfHi, cov(math.Hypot(px-(cx-hw*0.02), py-(cy-hh*0.12))-hw*0.12)*alpha)
}

// drawBrow 一根深色斜眉条（盖住眼上部辉光形成斜眉）。sign 区分左右（内端方向镜像）。
func drawBrow(acc *[3]float64, px, py, cx, browCy, angle, hw, alpha float64) {
	if alpha <= 0.02 {
		return
	}
	ca, sa := math.Cos(angle), math.Sin(angle)
	dx, dy := px-cx, py-browCy
	lx := dx*ca - dy*sa
	ly := dx*sa + dy*ca
	d := sdfRoundBox(lx, ly, 0, 0, hw*0.95, 7, 5)
	composite(acc, sdfBrow, cov(d)*alpha)
}

// render 逐像素渲染当前表情到 buf。
func (s *SDFFaceSource) render() {
	const (
		cx0, cy0 = 120.0, 120.0
		eyeDX    = 46.0
		scrR     = 119.0
	)
	fl := s.floatY()
	eyeY := 104.0 + fl
	mouthCy := 176.0 + fl
	blink := s.blinkVal()
	eyeOpen := clampf(s.cur.eyeOpen*(1-blink), 0, 1)
	col := s.cur.col
	br := s.breath()
	tilt := s.cur.tilt
	ct, stt := math.Cos(tilt), math.Sin(tilt)

	hw := 28.0 * s.cur.eyeScale
	hhFull := (40.0 + s.cur.tall*14) * s.cur.eyeScale
	// 上眼睑下压 + 开心眯 → 有效高度。
	hh := math.Max(4, hhFull*eyeOpen*(1-0.35*s.cur.lidTop)*(1-0.25*s.cur.squint))
	rC := math.Min(hw, hh) * 0.94 // 很圆的胶囊眼
	rScale := 1 - 0.12*s.cur.asym
	// 视线让整只眼轻微移动（看向别处），高光随之。
	gx := clampf(s.cur.gazeX+s.sacX, -1, 1)
	gy := clampf(s.cur.gazeY+s.sacY, -1, 1)
	lcx := cx0 - eyeDX + gx*6
	rcx := cx0 + eyeDX + gx*6
	ecy := eyeY + gy*6
	// 眉：位置在眼上沿附近，越 raise 越高；angry 压低盖住眼。
	browY := eyeY - hh*0.75 - s.cur.browRaise*12
	brow := s.cur.brow

	// 嘴：平滑曲线带，中间厚两端收尖。
	mHW := 21.0
	mArc := s.cur.mouthCurve * 12
	mOpen := s.mouthOpenEff() * 13

	for y := 0; y < scrH; y++ {
		fy := float64(y) - cy0
		for x := 0; x < scrW; x++ {
			fx := float64(x) - cx0
			px := cx0 + fx*ct - fy*stt
			py := cy0 + fx*stt + fy*ct
			rr := math.Hypot(fx, fy)
			acc := [3]float64{0, 0, 0}

			// 眼形（左右，含 asym 右眼略小）。
			dL := sdfRoundBox(px, py, lcx, ecy, hw, hh, rC)
			dR := sdfRoundBox(px, py, rcx, ecy, hw*rScale, hh*rScale, rC*rScale)

			// 嘴 SDF。
			dM := 1e9
			if py > eyeY+50 && py < eyeY+108 {
				u := (px - cx0) / mHW
				if uu := u * u; uu < 1 {
					edge := math.Sqrt(1 - uu)
					yc := mouthCy + mArc*(1-uu)
					band := (1.8+mOpen)*edge + 1.2
					dM = math.Abs(py-yc) - band
				}
			}

			// 辉光（眼 + 嘴）。
			addGlow(&acc, col, dL, 9, 0.9*br)
			addGlow(&acc, col, dR, 9, 0.9*br)
			if dM < 1e8 {
				addGlow(&acc, col, dM, 7, 0.72*br)
			}
			// 实体填充（实心色）。
			composite(&acc, col, cov(dL))
			composite(&acc, col, cov(dR))
			if dM < 1e8 {
				composite(&acc, col, cov(dM))
			}
			// 双高光。
			hiA := clampf(eyeOpen*(1-s.cur.squint), 0, 1)
			drawHighlights(&acc, px, py, lcx, ecy, hw, hh, hiA)
			drawHighlights(&acc, px, py, rcx, ecy, hw*rScale, hh*rScale, hiA)

			// 斜眉（深色，盖住眼上部）。左右内端方向镜像；angry 内端下压、sad/scared 内端上挑。
			if brow > 0.02 {
				drawBrow(&acc, px, py, lcx, browY, s.cur.browAngle, hw, brow)
				drawBrow(&acc, px, py, rcx, browY-s.cur.asym*6, -s.cur.browAngle, hw*rScale, brow)
			}

			// 圆屏遮罩。
			m := clampf(scrR-rr, 0, 1)
			i := (y*scrW + x) * 3
			s.buf[i] = byte(clampf(acc[0]*m, 0, 255))
			s.buf[i+1] = byte(clampf(acc[1]*m, 0, 255))
			s.buf[i+2] = byte(clampf(acc[2]*m, 0, 255))
		}
	}
}
