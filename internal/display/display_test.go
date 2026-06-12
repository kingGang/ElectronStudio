package display

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// TestSpriteSheet 验证精灵图集加载：一张大图按 meta 切成多帧。
func TestSpriteSheet(t *testing.T) {
	dir := t.TempDir()
	// 480×240 = 两帧 240×240（左红右蓝）。
	img := image.NewRGBA(image.Rect(0, 0, 480, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 480; x++ {
			if x < 240 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				img.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	f, _ := os.Create(filepath.Join(dir, "happy.png"))
	_ = png.Encode(f, img)
	_ = f.Close()
	_ = os.WriteFile(filepath.Join(dir, "happy.json"),
		[]byte(`{"frame_width":240,"frame_height":240,"frames":2,"fps":10}`), 0o600)

	clips, err := LoadClips(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	clip, ok := clips["happy"]
	if !ok || len(clip.Frames) != 2 || clip.FPS != 10 {
		t.Fatalf("切帧错误: %+v", clip)
	}
	// 第一帧应为红色（R=255,G=0,B=0）。
	if clip.Frames[0][0] != 255 || clip.Frames[0][1] != 0 {
		t.Fatalf("首帧颜色错误: %v", clip.Frames[0][:3])
	}
	// 第二帧应为蓝色（B=255）。
	if clip.Frames[1][2] != 255 {
		t.Fatalf("第二帧颜色错误: %v", clip.Frames[1][:3])
	}
}

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
	clips := map[string]Clip{"happy": {Frames: [][]byte{frame, frame}, FPS: 15}}
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
	comp := NewCompositor(nil, NewClipSource(map[string]Clip{}), NewEmotionSource())
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
