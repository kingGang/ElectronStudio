package display

import (
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// TestEmotionAnimates 验证程序动画脸会随时间产生变化帧（眨眼 / 说话口型）。
func TestEmotionAnimates(t *testing.T) {
	s := NewEmotionSource()
	s.SetSpeaking(true)
	distinct := 0
	for i := 0; i < 120; i++ { // 4 秒 @30fps
		if s.Frame() != nil {
			distinct++
		}
	}
	if distinct < 3 {
		t.Fatalf("说话时应产生多帧动画, 实际变化帧数 %d", distinct)
	}
}

// TestEmotionFrameSize 验证返回帧尺寸正确。
func TestEmotionFrameSize(t *testing.T) {
	s := NewEmotionSource()
	s.SetEmotion("happy")
	f := s.Frame()
	if len(f) != robot.ImageBytesRGB888 {
		t.Fatalf("帧尺寸错误: %d", len(f))
	}
}

// TestClipSourcePlays 验证素材片循环播放与回退。
func TestClipSourcePlays(t *testing.T) {
	frame := make([]byte, robot.ImageBytesRGB888)
	clips := map[string][][]byte{"happy": {frame, frame}}
	cs := NewClipSource(clips)

	if !cs.Has("happy") || cs.Has("sad") {
		t.Fatal("Has 判断错误")
	}
	cs.SetEmotion("sad") // 无素材 → nil
	if cs.Frame() != nil {
		t.Fatal("无素材情绪应返回 nil")
	}
	cs.SetEmotion("happy")
	if cs.Frame() == nil {
		t.Fatal("有素材情绪应返回帧")
	}
}

// TestCompositorFallback 验证：无素材时回退到程序动画脸。
func TestCompositorFallback(t *testing.T) {
	comp := NewCompositor(NewClipSource(map[string][][]byte{}), NewEmotionSource())
	comp.SetEmotion("happy")
	comp.SetSpeaking(true)
	// 应能从程序脸拿到帧。
	got := false
	for i := 0; i < 30; i++ {
		if comp.Frame() != nil {
			got = true
			break
		}
	}
	if !got {
		t.Fatal("无素材应回退到程序动画脸并产生帧")
	}
}
