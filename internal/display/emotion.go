package display

import (
	"fmt"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// 屏幕尺寸（圆形 LCD，240×240）。
const (
	scrW = robot.ScreenWidth
	scrH = robot.ScreenHeight
)

// 主色（青绿）与按情绪略变的强调色；背景黑以配合圆屏裁切。
var (
	colBG   = [3]byte{0, 0, 0}
	colEye  = [3]byte{0x00, 0xE5, 0xC7}
	colBlue = [3]byte{0x3B, 0x82, 0xF6}
	colRed  = [3]byte{0xEF, 0x44, 0x44}
)

// 说话时的口型节奏（嘴张开等级序列，约 10Hz 切换，看起来像在讲话）。
var talkPattern = []int{0, 2, 3, 1, 2, 0, 3, 1}

// EmotionSource 是【实时动画脸】：AI 决定情绪，主机每帧渲染——眨眼、说话时口型同步。
//
// 由 device.Driver 以 30fps 调用 Frame()；为省带宽，仅当画面实际变化时返回新帧
// （静止时返回 nil，设备/界面沿用上一帧）。
type EmotionSource struct {
	mu       sync.Mutex
	emotion  string
	speaking bool
	tick     int
	lastKey  string // 上次渲染的视觉特征，用于"变化才推帧"
	buf      []byte // 240×240×3 RGB888
}

// NewEmotionSource 创建一个动画表情源，初始 neutral。
func NewEmotionSource() *EmotionSource {
	return &EmotionSource{emotion: "neutral", buf: make([]byte, robot.ImageBytesRGB888)}
}

// SetEmotion 设定当前情绪（实现 Face）。
func (s *EmotionSource) SetEmotion(e string) {
	s.mu.Lock()
	s.emotion = e
	s.mu.Unlock()
}

// SetSpeaking 设定是否在说话（实现 Face）：true 时驱动口型动画。
func (s *EmotionSource) SetSpeaking(b bool) {
	s.mu.Lock()
	s.speaking = b
	s.mu.Unlock()
}

// Invalidate 让下一次 Frame() 必定重渲染一帧（用于从摄像头切回时覆盖屏上残留）。
func (s *EmotionSource) Invalidate() {
	s.mu.Lock()
	s.lastKey = ""
	s.mu.Unlock()
}

// Frame 实现 Source：推进动画一帧，仅在画面变化时返回新帧，否则 nil。
func (s *EmotionSource) Frame() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick++

	// 眨眼：约每 3.2s 闭眼 ~100ms。
	blink := (s.tick % 96) < 3
	// 口型：说话时按节奏张合，否则闭合。
	mouth := 0
	if s.speaking {
		mouth = talkPattern[(s.tick/3)%len(talkPattern)]
	}

	key := fmt.Sprintf("%s|%t|%d", s.emotion, blink, mouth)
	if key == s.lastKey {
		return nil // 画面未变，省带宽
	}
	s.lastKey = key
	s.render(blink, mouth)

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
func (s *EmotionSource) rect(cx, cy, hw, hh int, c [3]byte) {
	for y := cy - hh; y <= cy+hh; y++ {
		for x := cx - hw; x <= cx+hw; x++ {
			s.px(x, y, c)
		}
	}
}

// render 按情绪 + 眨眼 + 口型等级绘制一帧表情。
func (s *EmotionSource) render(blink bool, mouth int) {
	const (
		eyeLX, eyeRX = 80, 160
		eyeY         = 100
		mouthY       = 165
	)
	s.clear(colBG)

	eye := colEye
	switch s.emotion {
	case "angry":
		eye = colRed
	case "sad":
		eye = colBlue
	}

	// 眼睛：闭眼时画细线，否则画圆（surprised 更大）。
	eyeR := 22
	if s.emotion == "surprised" {
		eyeR = 26
	}
	if blink {
		s.rect(eyeLX, eyeY, eyeR, 2, eye)
		s.rect(eyeRX, eyeY, eyeR, 2, eye)
	} else {
		s.disc(eyeLX, eyeY, eyeR, eye)
		s.disc(eyeRX, eyeY, eyeR, eye)
		if s.emotion == "confused" { // 一大一小
			s.clear2eye(eyeRX, eyeY, eyeR, 16, eye)
		}
	}

	// 愤怒：斜眉。
	if s.emotion == "angry" {
		for i := 0; i < 36; i++ {
			s.px(eyeLX-20+i, eyeY-34+i/2, eye)
			s.px(eyeRX+20-i, eyeY-34+i/2, eye)
		}
	}

	// 嘴：基础形状随情绪，开口高度随口型等级（说话时变大）。
	open := mouth * 4
	switch s.emotion {
	case "happy":
		for i := 0; i < 16; i++ { // 上扬微笑
			s.rect(120, mouthY+i, 38-i*2, 1, eye)
		}
		if open > 0 {
			s.disc(120, mouthY+6, 6+open, eye)
		}
	case "sad":
		for i := 0; i < 16; i++ { // 下垂
			s.rect(120, mouthY+16-i, 38-i*2, 1, eye)
		}
	case "surprised":
		s.disc(120, mouthY+5, 16+open, eye) // 张大的圆嘴
	default: // neutral / angry / confused
		s.rect(120, mouthY, 32, 3+open, eye)
	}
}

// clear2eye 把右眼区域改画成更小的眼睛（confused 用），先抹掉再画小圆。
func (s *EmotionSource) clear2eye(cx, cy, bigR, smallR int, c [3]byte) {
	s.disc(cx, cy, bigR, colBG)
	s.disc(cx, cy, smallR, c)
}
