package display

import (
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// 屏幕尺寸（圆形 LCD，240×240）。
const (
	scrW = robot.ScreenWidth
	scrH = robot.ScreenHeight
)

// 主色（青绿，呼应 UI 主题）与背景（黑，配合圆屏裁切）。
var (
	colBG   = [3]byte{0, 0, 0}
	colEye  = [3]byte{0x00, 0xE5, 0xC7}
	colBlue = [3]byte{0x3B, 0x82, 0xF6}
	colRed  = [3]byte{0xEF, 0x44, 0x44}
)

// EmotionSource 把"情绪"渲染成一张极简表情（眼睛 + 嘴）。
// 这是纯 Go、无素材依赖的占位实现，用于验证"主机生成帧 → 设备屏 + UI 镜像"同步管线；
// 之后可替换为预渲染的表情帧序列或摄像头帧。
type EmotionSource struct {
	mu      sync.Mutex
	emotion string
	dirty   bool
	buf     []byte // 240×240×3 RGB888
}

// NewEmotionSource 创建一个表情画面源，初始为 neutral 并已渲染首帧。
func NewEmotionSource() *EmotionSource {
	s := &EmotionSource{
		emotion: "neutral",
		dirty:   true,
		buf:     make([]byte, robot.ImageBytesRGB888),
	}
	s.render()
	return s
}

// SetEmotion 切换情绪并重渲染（仅在变化时标记为脏帧）。
func (s *EmotionSource) SetEmotion(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e == s.emotion {
		return
	}
	s.emotion = e
	s.render()
	s.dirty = true
}

// Frame 实现 Source：有新帧时返回其副本，否则返回 nil。
func (s *EmotionSource) Frame() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	s.dirty = false
	out := make([]byte, len(s.buf))
	copy(out, s.buf)
	return out
}

// ---- 像素绘制工具 ----

func (s *EmotionSource) clear(c [3]byte) {
	for i := 0; i < len(s.buf); i += 3 {
		s.buf[i], s.buf[i+1], s.buf[i+2] = c[0], c[1], c[2]
	}
}

func (s *EmotionSource) px(x, y int, c [3]byte) {
	if x < 0 || y < 0 || x >= scrW || y >= scrH {
		return
	}
	i := (y*scrW + x) * 3
	s.buf[i], s.buf[i+1], s.buf[i+2] = c[0], c[1], c[2]
}

// disc 画一个实心圆。
func (s *EmotionSource) disc(cx, cy, r int, c [3]byte) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				s.px(x, y, c)
			}
		}
	}
}

// rect 画一个实心矩形（以中心 + 半宽半高表示）。
func (s *EmotionSource) rect(cx, cy, hw, hh int, c [3]byte) {
	for y := cy - hh; y <= cy+hh; y++ {
		for x := cx - hw; x <= cx+hw; x++ {
			s.px(x, y, c)
		}
	}
}

// render 按当前情绪绘制一张表情到 buf。
// 眼睛固定在上方两侧，嘴/眉随情绪变化；颜色随情绪略变以增强区分度。
func (s *EmotionSource) render() {
	const (
		eyeLX, eyeRX = 80, 160
		eyeY         = 100
		mouthY       = 165
	)
	s.clear(colBG)
	eye := colEye
	if s.emotion == "angry" {
		eye = colRed
	} else if s.emotion == "sad" {
		eye = colBlue
	}

	switch s.emotion {
	case "happy":
		s.disc(eyeLX, eyeY, 24, eye)
		s.disc(eyeRX, eyeY, 24, eye)
		// 上扬的嘴：用一段下半弧（多排逐渐变窄）近似微笑。
		for i := 0; i < 18; i++ {
			s.rect(120, mouthY+i, 40-i*2, 1, eye)
		}
	case "sad":
		s.disc(eyeLX, eyeY, 22, eye)
		s.disc(eyeRX, eyeY, 22, eye)
		// 下垂的嘴。
		for i := 0; i < 18; i++ {
			s.rect(120, mouthY+18-i, 40-i*2, 1, eye)
		}
	case "angry":
		s.disc(eyeLX, eyeY, 20, eye)
		s.disc(eyeRX, eyeY, 20, eye)
		// 斜眉。
		for i := 0; i < 36; i++ {
			s.px(eyeLX-20+i, eyeY-32+i/2, eye)
			s.px(eyeRX+20-i, eyeY-32+i/2, eye)
		}
		s.rect(120, mouthY, 35, 5, eye)
	case "surprised":
		s.disc(eyeLX, eyeY, 26, eye)
		s.disc(eyeRX, eyeY, 26, eye)
		s.disc(120, mouthY+5, 22, eye) // 张大的圆嘴
	case "confused":
		s.disc(eyeLX, eyeY, 24, eye)
		s.disc(eyeRX, eyeY, 16, eye) // 一大一小
		s.rect(120, mouthY, 30, 4, eye)
	default: // neutral
		s.disc(eyeLX, eyeY, 22, eye)
		s.disc(eyeRX, eyeY, 22, eye)
		s.rect(120, mouthY, 35, 4, eye)
	}
}
