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
	// mono=true：按【黑白眼睛类】风格画——纯白眼、不发光、不打高光，只留"实心白形状 + 深色眉刻痕"，
	// 与官方那套黑底白眼 GIF 同一观感。用于「黑白眼睛类」缺失的表情（色色/鬼畜/睡着了…）由程序补齐，
	// 而不是直接套类型B的彩色脸、跟旁边的 GIF 风格打架。false=类型B（实心彩色眼+辉光+高光）。
	mono bool
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
	asym      float64 // 左右不对称（疑惑：右眉更高）
	wink      float64 // 眨单眼（左眼闭）——winking
	tears     float64 // 眼下泪滴——crying
	shades    float64 // 墨镜横条盖眼——cool
	sweat     float64 // 尴尬汗滴——embarrassed
	smirk     float64 // 坏笑：嘴一角上扬——confident
	mouthCurve float64 // 嘴角 -1皱..+1笑（0=平直短横）
	mouthOpen  float64 // 0..1 张嘴基线
	tilt       float64 // 头倾（疑惑/俏皮）
	col        [3]float64 // 眼/嘴主色（实心）
}

var (
	sdfHi   = [3]float64{240, 255, 254} // 高光白
	sdfBrow = [3]float64{40, 48, 66}    // 眉条深色（盖住眼上部的辉光，形成斜眉）
	// sdfMonoEye 是【黑白眼睛类】的眼/嘴色：纯白。配合关掉辉光与高光，得到官方那套黑底白眼的干净观感。
	sdfMonoEye = [3]float64{248, 250, 252}
)

// squintCarveMax 是眯眼挖弧的深度上限。挖弧从眼下方切进去，squint 越大留下的白月牙越薄，
// 到 0.8 以上就把眼睛整个挖穿了（彩色只剩辉光的空心圆环、黑白只剩一条细弧）。
// 情绪表里 laughing=0.8、lol=1 都会穿，故在渲染时统一封顶——保留 emotionTarget 里的语义值不动。
const squintCarveMax = 0.6

// emotionColor 给出情绪的实心主色（照参考图）。
func emotionColor(e string) [3]float64 {
	switch e {
	case "sad", "crying":
		return [3]float64{150, 200, 247} // 淡蓝
	case "angry":
		return [3]float64{248, 118, 90} // 红橙（更红更凶）
	case "surprised", "shocked":
		return [3]float64{245, 236, 156} // 亮黄
	case "confused", "thinking":
		return [3]float64{212, 180, 247} // 淡紫
	case "loving":
		return [3]float64{248, 150, 190} // 粉
	case "embarrassed":
		return [3]float64{248, 176, 168} // 暖粉（脸红）
	case "sleepy":
		return [3]float64{182, 194, 236} // 柔蓝紫
	case "silly":
		return [3]float64{150, 246, 150} // 亮绿
	case "naughty":
		return [3]float64{250, 130, 205} // 艳粉（色色，比 loving 更骚）
	case "asleep":
		return [3]float64{150, 165, 215} // 深柔蓝紫（比 sleepy 更沉、更静）
	case "tired":
		return [3]float64{172, 182, 202} // 灰蓝（没电了）
	case "manic":
		return [3]float64{255, 110, 220} // 亮品红（鬼畜）
	case "speechless":
		return [3]float64{190, 200, 210} // 灰（无语）
	case "scared":
		return [3]float64{198, 228, 250} // 惨白蓝（吓的）
	default: // neutral / happy / laughing / lol / funny / cool / confident / winking
		return [3]float64{124, 240, 240} // 青
	}
}

// emotionTarget 给出某情绪的目标表情参数（照参考图；刻意夸张）。
func emotionTarget(e string) faceParams {
	p := faceParams{eyeOpen: 1, eyeScale: 1}
	switch e {
	case "happy":
		p.mouthCurve = 0.9
		p.squint = 0.35
	case "laughing":
		p.squint = 0.8 // ∩∩ 眯眼大笑
		p.mouthCurve = 0.9
		p.mouthOpen = 0.6
	case "funny":
		p.squint = 0.6
		p.mouthCurve = 1.0
		p.mouthOpen = 0.45
		p.tilt = 0.08
	case "sad":
		p.mouthCurve = -0.55
		p.brow = 1
		p.browAngle = 0.7
		p.browRaise = 0.2
		p.gazeY = 0.18
		p.eyeOpen = 0.9
	case "angry":
		p.mouthCurve = -0.5
		p.brow = 1
		p.browAngle = -0.9 // 更凶
		p.browRaise = -0.35
		p.eyeScale = 0.96
	case "crying":
		p.mouthCurve = -0.62
		p.mouthOpen = 0.32
		p.brow = 1
		p.browAngle = 0.6
		p.browRaise = 0.15
		p.tears = 1
		p.eyeOpen = 0.82
		p.gazeY = 0.2
	case "loving":
		p.mouthCurve = 0.85
		p.squint = 0.28
		p.eyeScale = 1.06
	case "embarrassed":
		p.mouthCurve = -0.12
		p.squint = 0.22
		p.gazeX = 0.42 // 别开眼
		p.sweat = 1
	case "surprised":
		p.tall = 0.2
		p.mouthOpen = 0.55
	case "shocked":
		p.tall = 0.38
		p.eyeScale = 1.12
		p.mouthOpen = 0.88 // 大张
	case "thinking":
		p.gazeX = 0.5 // 往右上想
		p.gazeY = -0.4
		p.brow = 1
		p.browAngle = -0.12
		p.browRaise = 0.3
		p.asym = 1
		p.mouthCurve = -0.05
		p.tilt = 0.06
	case "cool":
		p.shades = 1 // 墨镜
		p.mouthCurve = 0.28
		p.smirk = 0.6
	case "confident":
		p.lidTop = 0.28 // 半眯
		p.smirk = 1     // 坏笑
		p.mouthCurve = 0.4
		p.brow = 1
		p.browAngle = -0.1
		p.browRaise = 0.12
		p.asym = 1
	case "sleepy":
		p.lidTop = 0.6 // 眼皮沉
		p.mouthOpen = 0.15
		p.gazeY = 0.22
	case "winking":
		p.wink = 1 // 眨左眼
		p.mouthCurve = 0.65
		p.squint = 0.15
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
	case "lol": // 哈哈大笑：比 laughing 再狠一档——眼挤成 ∩∩、嘴大张、笑出眼泪、笑到歪头
		p.squint = 1
		p.mouthCurve = 1
		p.mouthOpen = 0.95
		p.tears = 0.65
		p.tilt = 0.12
		p.eyeScale = 1.05
	case "naughty": // 色色：半眯的色眯眯眼 + 单边坏笑 + 左右不对称
		p.lidTop = 0.5
		p.smirk = 1
		p.mouthCurve = 0.45
		p.mouthOpen = 0.2
		p.squint = 0.25
		p.brow = 1
		p.browAngle = -0.2
		p.browRaise = -0.05
		p.asym = 1
	case "asleep": // 睡着了：闭眼（eyeOpen→0，渲染里被 Max(4) 兜成一条细线）+ 微张嘴安睡
		p.eyeOpen = 0.05
		p.mouthOpen = 0.18
		p.mouthCurve = 0.15
		p.gazeY = 0.1
	case "tired": // 累了：眼皮很沉但还睁着 + 八字眉 + 叹气 + 冒汗（比 sleepy 更"没电"而非"想睡"）
		p.lidTop = 0.62
		p.eyeOpen = 0.85
		p.brow = 1
		p.browAngle = 0.55
		p.browRaise = -0.15
		p.mouthCurve = -0.35
		p.mouthOpen = 0.14
		p.gazeY = 0.28
		p.sweat = 0.7
	case "manic": // 鬼畜：斗鸡 + 左右不一 + 瞪大 + 狂张嘴 + 歪头，整个抽风感
		p.cross = 0.6
		p.asym = 1
		p.tall = 0.3
		p.eyeScale = 1.12
		p.mouthOpen = 0.85
		p.mouthCurve = 0.75
		p.tilt = 0.18
		p.brow = 1
		p.browAngle = -0.6
		p.browRaise = 0.35
	case "speechless": // 无语：死鱼眼(半眯) + 嘴平直 + 斜眼看别处 + 一滴汗
		p.lidTop = 0.7
		p.eyeOpen = 0.9
		p.mouthCurve = 0
		p.gazeX = 0.35
		p.sweat = 0.8
	case "scared": // 害怕：竖长瞪大 + 高高的八字眉 + 张嘴 + 冒汗
		p.tall = 0.32
		p.eyeScale = 1.15
		p.brow = 1
		p.browAngle = 0.65
		p.browRaise = 0.6
		p.mouthOpen = 0.5
		p.mouthCurve = -0.45
		p.sweat = 1
	}
	p.col = emotionColor(e)
	return p
}

// RenderEmotionThumb 渲染某情绪【缓动收敛后】的 SDF 表情单帧（RGB888，240×240）。
// mono=true 按黑白眼睛类风格（纯白眼、无辉光高光）画，用于给「黑白眼睛类」缺的表情补图；
// false 则是类型B（实心彩色眼+辉光+高光）。
//
// 供素材页当缩略图用：没有 GIF 素材的情绪（新加的表情、silly 等）在界面上也能看到它长什么样。
// 用【独立实例】渲染，绝不碰正在驱动屏幕的那张脸；先空跑够帧数让参数缓动收敛到目标表情，
// 再强制清掉眨眼状态渲染最后一帧，避免正好抓到闭眼的那一瞬（asleep 除外——它本来就闭眼）。
func RenderEmotionThumb(emotion string, mono bool) []byte {
	s := NewSDFFaceSource()
	s.SetMono(mono)
	s.SetEmotion(emotion)
	for i := 0; i < 80; i++ { // 跑够帧数让缓动收敛（指数缓动，80 帧足够到位）
		_ = s.Frame()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blinkT = 0 // 别抓到眨眼闭眼的一帧
	s.render()
	out := make([]byte, len(s.buf))
	copy(out, s.buf)
	return out
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

// SetMono 切换渲染风格：true=黑白眼睛类（纯白眼、无辉光无高光，贴合官方黑底白眼素材的观感）；
// false=类型B（实心彩色眼+辉光+高光）。「黑白眼睛类」缺的表情由它按黑白风格补齐，风格才不打架。
func (s *SDFFaceSource) SetMono(on bool) {
	s.mu.Lock()
	changed := s.mono != on
	s.mono = on
	if changed {
		s.haveLast = false // 风格变了：强制下一帧重画（visualKey 也含 mono，双保险）
	}
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
	c.wink = e(c.wink, t.wink)
	c.tears = e(c.tears, t.tears)
	c.shades = e(c.shades, t.shades)
	c.sweat = e(c.sweat, t.sweat)
	c.smirk = e(c.smirk, t.smirk)
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
	if s.mono { // 风格也算进指纹：黑白/彩色切换必须重画
		add(1)
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
	add(s.cur.wink)
	add(s.cur.tears)
	add(s.cur.shades)
	add(s.cur.sweat)
	add(s.cur.smirk)
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
	mono := s.mono
	col := s.cur.col
	if mono {
		col = sdfMonoEye // 黑白眼睛类：一律纯白眼，不用情绪彩色（辉光/高光在下面也会关掉）
	}
	br := s.breath()
	tilt := s.cur.tilt
	ct, stt := math.Cos(tilt), math.Sin(tilt)

	hw := 28.0 * s.cur.eyeScale
	hhFull := (40.0 + s.cur.tall*14) * s.cur.eyeScale
	hf := hhFull * (1 - 0.35*s.cur.lidTop) // 睑压系数（眯眼靠下方挖弧成 ∩，不在这里压高）
	rScale := 1 - 0.12*s.cur.asym
	eyeOpenL := clampf(eyeOpen*(1-s.cur.wink), 0, 1) // winking：左眼闭
	hwL, hhL := hw, math.Max(4, hf*eyeOpenL)
	hwR, hhR := hw*rScale, math.Max(4, hf*eyeOpen*rScale)
	rCL := math.Min(hwL, hhL) * 0.94
	rCR := math.Min(hwR, hhR) * 0.94
	// 视线让整只眼轻微移动（看向别处），高光随之。
	gx := clampf(s.cur.gazeX+s.sacX, -1, 1)
	gy := clampf(s.cur.gazeY+s.sacY, -1, 1)
	lcx := cx0 - eyeDX + gx*6
	rcx := cx0 + eyeDX + gx*6
	ecy := eyeY + gy*6
	browY := eyeY - hhR*0.75 - s.cur.browRaise*12
	brow := s.cur.brow
	mHW := 21.0
	mArc := s.cur.mouthCurve * 12
	mOpen := s.mouthOpenEff() * 13
	smirk, tears, shades, sweat := s.cur.smirk, s.cur.tears, s.cur.shades, s.cur.sweat
	drop := float64(s.tick%42) * 0.5 // 泪/汗下落
	tearCol := [3]float64{150, 214, 255}
	if mono {
		tearCol = sdfMonoEye // 黑白眼睛类：泪滴/汗滴也必须是白的，否则蓝色会漏进黑白风格里
	}

	for y := 0; y < scrH; y++ {
		fy := float64(y) - cy0
		for x := 0; x < scrW; x++ {
			fx := float64(x) - cx0
			px := cx0 + fx*ct - fy*stt
			py := cy0 + fx*stt + fy*ct
			rr := math.Hypot(fx, fy)
			acc := [3]float64{0, 0, 0}

			dL := sdfRoundBox(px, py, lcx, ecy, hwL, hhL, rCL)
			dR := sdfRoundBox(px, py, rcx, ecy, hwR, hhR, rCR)
			// 开心/大笑：下方挖上升的弧，把眼变成 ∩ 眯眼笑。
			// 挖弧越深、剩下的月牙越薄：squint≥0.8 会把眼睛整个挖穿——彩色下只剩辉光的空心
			// 圆环，黑白下只剩一条细弧。两种类型都得封顶到"还留得住厚实 ∩"的程度
			// （squint=0.6 的 funny 是好的，0.8 的 laughing、1 的 lol 就穿了）。
			sq := math.Min(s.cur.squint, squintCarveMax)
			if sq > 0.08 {
				dL = math.Max(dL, -(math.Hypot(px-lcx, py-(ecy+hhL*(1.15-0.85*sq))) - hhL*1.3))
				dR = math.Max(dR, -(math.Hypot(px-rcx, py-(ecy+hhR*(1.15-0.85*sq))) - hhR*1.3))
			}

			// 嘴 SDF（smirk 抬右嘴角成坏笑）。
			dM := 1e9
			if py > eyeY+48 && py < eyeY+112 {
				u := (px - cx0) / mHW
				if uu := u * u; uu < 1 {
					edge := math.Sqrt(1 - uu)
					yc := mouthCy + mArc*(1-uu) - smirk*math.Max(u, 0)*9
					band := (1.8+mOpen)*edge + 1.2
					dM = math.Abs(py-yc) - band
				}
			}

			// 黑白眼睛类(mono)：不发光、不打高光——只要"实心白形状"，才和官方那套黑底白眼 GIF 同观感。
			if !mono {
				addGlow(&acc, col, dL, 9, 0.9*br)
				addGlow(&acc, col, dR, 9, 0.9*br)
				if dM < 1e8 {
					addGlow(&acc, col, dM, 7, 0.72*br)
				}
			}
			composite(&acc, col, cov(dL))
			composite(&acc, col, cov(dR))
			if dM < 1e8 {
				composite(&acc, col, cov(dM))
			}
			if !mono {
				drawHighlights(&acc, px, py, lcx, ecy, hwL, hhL, clampf(eyeOpenL*(1-s.cur.squint), 0, 1))
				drawHighlights(&acc, px, py, rcx, ecy, hwR, hhR, clampf(eyeOpen*(1-s.cur.squint), 0, 1))
			}

			// 泪滴（crying）：两眼下方蓝色小滴，缓缓下落。
			if tears > 0.05 {
				composite(&acc, tearCol, cov(sdfEllipse(px, py, lcx, ecy+hhL+4+drop, 4, 6))*tears)
				composite(&acc, tearCol, cov(sdfEllipse(px, py, rcx, ecy+hhR+4+drop, 4, 6))*tears)
			}

			// 斜眉（深色，盖住眼上部）。
			if brow > 0.02 {
				drawBrow(&acc, px, py, lcx, browY, s.cur.browAngle, hwL, brow)
				drawBrow(&acc, px, py, rcx, browY-s.cur.asym*6, -s.cur.browAngle, hwR, brow)
			}

			// 墨镜（cool）：深色镜片盖眼 + 鼻梁横条。
			if shades > 0.05 {
				dl := sdfRoundBox(px, py, lcx, ecy-hhR*0.08, hwL*0.98, hhR*0.62, 9)
				dr := sdfRoundBox(px, py, rcx, ecy-hhR*0.08, hwR*0.98, hhR*0.62, 9)
				bridge := sdfRoundBox(px, py, cx0+gx*6, ecy-hhR*0.28, eyeDX-hw*0.75, 3.5, 3)
				composite(&acc, sdfBrow, cov(math.Min(math.Min(dl, dr), bridge))*shades)
			}

			// 汗滴（embarrassed）：右上角一滴。
			if sweat > 0.05 {
				composite(&acc, tearCol, cov(sdfEllipse(px, py, cx0+58, eyeY-24+drop*0.6, 4, 6))*sweat)
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
