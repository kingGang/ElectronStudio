package display

import (
	"math"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// SDFFaceSource 是基于【有向距离场(SDF)】的实时表情脸：每帧逐像素求各五官的 SDF、抗锯齿合成，
// 与片元着色器(fragment shader)做的是同一套数学，只是在 CPU 上算（本工程 CGO_ENABLED=0、要交叉
// 编到树莓派，用不了 GPU）。240×240 逐像素在桌面/树莓派上跑 30fps 都很轻松。
//
// 相比老的 EmotionSource（硬边圆/矩形直接切换），SDF 的好处是【边缘抗锯齿】且表情可以在情绪之间
// 【连续 morph】：一组连续表情参数向目标情绪缓动，眨眼、眯眼、挑眉、嘴角都平滑过渡。
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
	blinkT    int // >0 表示正在眨眼；从 1 递增到 blinkDur
	blinkDur  int
	cur       faceParams
	buf       []byte // 240×240×3 RGB888
	lastKey   uint64
	haveLast  bool
}

// faceParams 是一组连续表情参数；每种情绪对应一组目标值，当前值每帧向目标缓动。
type faceParams struct {
	eyeOpen   float64 // 0..1 睁眼度（眨眼→0）
	squint    float64 // 0..1 下眼睑上抬，开心眯眼 ∩∩
	eyeScale  float64 // 眼整体大小（surprised 更大）
	pupil     float64 // 瞳孔占眼半径比
	browAngle float64 // 眉内端：+ 上挑(悲伤/无辜)，- 下压(愤怒)
	browRaise float64 // 眉整体上抬(surprised)
	gazeX     float64 // 视线水平偏移 -1..1
	gazeY     float64 // 视线垂直偏移 -1..1
	mouthCurve float64 // 嘴角 -1皱..+1笑
	mouthOpen  float64 // 0..1 张嘴基线(surprised)
	tilt       float64 // 头部倾斜(弧度，confused)
	asym       float64 // 左右不对称强度(confused: 右眉更挑/右眼略小)
	col        [3]float64 // 主色 RGB 0..255
}

var (
	sdfCyan = [3]float64{0x00, 0xE5, 0xC7}
	sdfBlue = [3]float64{0x3B, 0x82, 0xF6}
	sdfRed  = [3]float64{0xEF, 0x44, 0x44}
	sdfDark = [3]float64{0x00, 0x2a, 0x25} // 瞳孔
	sdfLite = [3]float64{0xE8, 0xFF, 0xFC} // 高光
)

// emotionTarget 给出某情绪的目标表情参数。
func emotionTarget(e string) faceParams {
	p := faceParams{eyeOpen: 1, eyeScale: 1, pupil: 0.42, mouthCurve: 0.06, col: sdfCyan}
	switch e {
	case "happy":
		p.squint = 0.72
		p.mouthCurve = 0.95
		p.browRaise = 0.18
	case "sad":
		p.browAngle = 0.85
		p.gazeY = 0.30
		p.mouthCurve = -0.72
		p.eyeOpen = 0.80
		p.col = sdfBlue
	case "angry":
		p.browAngle = -0.95
		p.mouthCurve = -0.28
		p.eyeScale = 0.95
		p.pupil = 0.34
		p.col = sdfRed
	case "surprised":
		p.eyeScale = 1.32
		p.browRaise = 0.95
		p.mouthOpen = 0.75
		p.pupil = 0.5
	case "confused":
		p.browAngle = -0.22
		p.browRaise = 0.35
		p.tilt = 0.15
		p.asym = 1
		p.mouthCurve = -0.12
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

// SetEmotion 设定情绪（实现 Face）。目标参数由 step() 每帧缓动逼近。
func (s *SDFFaceSource) SetEmotion(e string) {
	s.mu.Lock()
	s.emotion = e
	s.mu.Unlock()
}

// SetSpeaking 设定是否说话（实现 Face）：true 时驱动口型。停说话时清零音量。
func (s *SDFFaceSource) SetSpeaking(b bool) {
	s.mu.Lock()
	s.speaking = b
	if !b {
		s.level = 0
	}
	s.mu.Unlock()
}

// SetMouthLevel 喂入实时口型音量(0..1)，让嘴按真实说话音量张合、与对话同步。
// 上层(如实时语音下行 PCM 的 RMS)可高频调用；一段时间不喂会自动衰减。
func (s *SDFFaceSource) SetMouthLevel(l float64) {
	s.mu.Lock()
	s.level = clampf(l, 0, 1)
	s.levelTick = s.tick
	s.mu.Unlock()
}

// Invalidate 让下一帧必定重渲染（从摄像头切回时覆盖残留）。
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
		if math.Abs(tgt-n) < 0.004 { // 足够近就吸附到目标，让静止帧稳定去重
			return tgt
		}
		return n
	}
	c := &s.cur
	c.eyeOpen = e(c.eyeOpen, t.eyeOpen)
	c.squint = e(c.squint, t.squint)
	c.eyeScale = e(c.eyeScale, t.eyeScale)
	c.pupil = e(c.pupil, t.pupil)
	c.browAngle = e(c.browAngle, t.browAngle)
	c.browRaise = e(c.browRaise, t.browRaise)
	c.gazeX = e(c.gazeX, t.gazeX)
	c.gazeY = e(c.gazeY, t.gazeY)
	c.mouthCurve = e(c.mouthCurve, t.mouthCurve)
	c.mouthOpen = e(c.mouthOpen, t.mouthOpen)
	c.tilt = e(c.tilt, t.tilt)
	c.asym = e(c.asym, t.asym)
	for i := 0; i < 3; i++ {
		c.col[i] = e(c.col[i], t.col[i])
	}

	// 眨眼：到点起一次，历时 blinkDur 帧的正弦闭合；结束后排下一次(2~3.5s 抖动)。
	if s.blinkT > 0 {
		s.blinkT++
		if s.blinkT > s.blinkDur {
			s.blinkT = 0
			s.nextBlink = s.tick + 65 + (s.tick*13)%40
		}
	} else if s.tick >= s.nextBlink {
		s.blinkT = 1
	}
	// 口型音量衰减：一段时间没喂新值就回落到 0（说话结束嘴自然合上）。
	if s.tick-s.levelTick > 5 && s.level > 0 {
		s.level = s.level * 0.7
		if s.level < 0.02 {
			s.level = 0
		}
	}
}

// blinkVal 返回当前眨眼闭合度 0..1（1=全闭）。
func (s *SDFFaceSource) blinkVal() float64 {
	if s.blinkT <= 0 {
		return 0
	}
	return math.Sin(math.Pi * float64(s.blinkT) / float64(s.blinkDur+1))
}

// mouthOpenEff 返回当前有效张嘴 0..1：说话时取(真实音量 or 节奏)与情绪基线的较大者。
func (s *SDFFaceSource) mouthOpenEff() float64 {
	mo := s.cur.mouthOpen
	if s.speaking || s.level > 0.02 {
		sp := 0.12 + 0.30*(0.5+0.5*math.Sin(float64(s.tick)*0.9)) // 无音量时的节奏兜底
		if s.level > 0.02 {
			sp = clampf(s.level*1.4, 0, 1)
		}
		if sp > mo {
			mo = sp
		}
	}
	return mo
}

// visualKey 把当前可见状态量化成一个键，用于"变化才推帧"。近似即可：键相同则视觉几乎一致。
func (s *SDFFaceSource) visualKey() uint64 {
	h := uint64(1469598103934665603)
	add := func(f float64) {
		q := uint64(int64(math.Round(f*48)) + (1 << 24))
		h = (h ^ q) * 1099511628211
	}
	eyeOpenEff := s.cur.eyeOpen * (1 - s.blinkVal())
	add(eyeOpenEff)
	add(s.cur.squint)
	add(s.cur.eyeScale)
	add(s.cur.pupil)
	add(s.cur.browAngle)
	add(s.cur.browRaise)
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

// ---- SDF 基元与工具（2D，返回有向距离，负=内部；对抗锯齿够用）----

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

// cov 由有向距离得到覆盖率(抗锯齿)：约 1px 过渡带。
func cov(d float64) float64 { return clampf(0.5-d, 0, 1) }

// render 逐像素渲染当前表情到 buf。
func (s *SDFFaceSource) render() {
	const (
		cx0, cy0 = 120.0, 120.0
		eyeY     = 100.0
		eyeDX    = 40.0
		mouthCy  = 166.0
		scrR     = 119.0
	)
	blink := s.blinkVal()
	eyeOpenEff := clampf(s.cur.eyeOpen*(1-blink), 0, 1)
	mo := s.mouthOpenEff()
	col := s.cur.col
	tilt := s.cur.tilt
	ct, st := math.Cos(tilt), math.Sin(tilt)

	// 眼参数（右眼受 asym 略缩小）。
	erx := 25.0 * s.cur.eyeScale
	eryFull := 30.0 * s.cur.eyeScale
	ery := math.Max(2.0, eryFull*eyeOpenEff)
	rScale := 1 - 0.22*s.cur.asym

	// 眉端点（内端受 browAngle 上/下，右眉受 asym 更挑）。
	browY := eyeY - 40 - s.cur.browRaise*10
	innerDY := s.cur.browAngle * 12
	// 左眉：外(62)→内(96)
	lbOx, lbOy, lbIx, lbIy := 62.0, browY, 96.0, browY-innerDY
	// 右眉：内(144)→外(178)，整体再上抬 asym*8
	rby := browY - s.cur.asym*8
	rbIx, rbIy, rbOx, rbOy := 144.0, rby-innerDY, 178.0, rby

	// 嘴：沿抛物线采 13 点成折线 + 张嘴椭圆(union)。
	const mn = 13
	var mx, my [mn]float64
	mCenterY := mouthCy + s.cur.mouthCurve*16
	for i := 0; i < mn; i++ {
		u := float64(i)/(mn-1)*2 - 1 // -1..1
		mx[i] = cx0 + u*22
		my[i] = mouthCy + s.cur.mouthCurve*16*(1-u*u)
	}
	openRy := mo * 22

	for y := 0; y < scrH; y++ {
		fy := float64(y) - cy0
		for x := 0; x < scrW; x++ {
			fx := float64(x) - cx0
			// 头部倾斜：把采样点反旋转到脸坐标。
			px := cx0 + fx*ct - fy*st
			py := cy0 + fx*st + fy*ct
			acc := [3]float64{0, 0, 0}

			// 嘴（仅嘴带内计算，省算力）。
			if py > 138 && py < 200 {
				dm := 1e9
				for i := 0; i < mn-1; i++ {
					if dd := sdfSeg(px, py, mx[i], my[i], mx[i+1], my[i+1]); dd < dm {
						dm = dd
					}
				}
				dm -= 3.0 // 线宽
				if openRy > 0.5 {
					if do := sdfEllipse(px, py, cx0, mCenterY, 19, openRy); do < dm {
						dm = do
					}
				}
				composite(&acc, col, cov(dm))
			}

			// 左右眼 + 眉（仅上半带）。
			if py < 155 {
				drawEye(&acc, px, py, cx0-eyeDX, eyeY, erx, ery, eryFull, s.cur.squint,
					s.cur.pupil, s.cur.gazeX, s.cur.gazeY, eyeOpenEff, col)
				drawEye(&acc, px, py, cx0+eyeDX, eyeY, erx*rScale, ery*rScale, eryFull*rScale, s.cur.squint,
					s.cur.pupil, s.cur.gazeX, s.cur.gazeY, eyeOpenEff, col)
				composite(&acc, col, cov(sdfSeg(px, py, lbOx, lbOy, lbIx, lbIy)-3.5))
				composite(&acc, col, cov(sdfSeg(px, py, rbIx, rbIy, rbOx, rbOy)-3.5))
			}

			// 圆屏遮罩：屏坐标半径外淡出到黑。
			rr := math.Hypot(fx, fy)
			m := clampf(scrR-rr, 0, 1)
			i := (y*scrW + x) * 3
			s.buf[i] = byte(clampf(acc[0]*m, 0, 255))
			s.buf[i+1] = byte(clampf(acc[1]*m, 0, 255))
			s.buf[i+2] = byte(clampf(acc[2]*m, 0, 255))
		}
	}
}

// drawEye 画一只眼：椭圆眼白(青) + 开心眯眼下切 + 瞳孔 + 高光。
func drawEye(acc *[3]float64, px, py, ecx, ecy, erx, ery, eryFull, squint, pupil, gazeX, gazeY, eyeOpen float64, col [3]float64) {
	// 快速包围盒剔除。
	if px < ecx-erx-3 || px > ecx+erx+3 || py < ecy-ery-3 || py > ecy+ery+3 {
		return
	}
	d := sdfEllipse(px, py, ecx, ecy, erx, ery)
	if squint > 0.02 { // 从下方挖一个上升的圆，形成开心 ∩ 眼
		carveCy := ecy + ery*(1.35-1.0*squint)
		dc := math.Hypot(px-ecx, py-carveCy) - ery*1.4
		if -dc > d {
			d = -dc
		}
	}
	composite(acc, col, cov(d))

	// 瞳孔 + 高光：睁眼且没眯眼时才画。
	alpha := clampf(eyeOpen*(1-squint*1.2), 0, 1)
	if alpha > 0.02 {
		pr := pupil * erx
		pcx := ecx + gazeX*(erx*0.35)
		pcy := ecy + gazeY*(eryFull*0.35)
		composite(acc, sdfDark, cov(sdfEllipse(px, py, pcx, pcy, pr, pr*1.05))*alpha)
		composite(acc, sdfLite, cov(math.Hypot(px-(pcx-pr*0.4), py-(pcy-pr*0.5))-pr*0.34)*alpha)
	}
}

// composite 用覆盖率把 col 叠到 acc 上（普通 over 混合）。
func composite(acc *[3]float64, col [3]float64, c float64) {
	if c <= 0 {
		return
	}
	acc[0] = lerpf(acc[0], col[0], c)
	acc[1] = lerpf(acc[1], col[1], c)
	acc[2] = lerpf(acc[2], col[2], c)
}
