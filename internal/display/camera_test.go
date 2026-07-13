package display

import (
	"bytes"
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// TestCameraReadFrames 验证：从原始 rgb24 流按帧长读取，Frame() 依次返回各帧、无新帧返回 nil。
func TestCameraReadFrames(t *testing.T) {
	n := robot.ImageBytesRGB888
	// 构造两帧：全 0x11 与全 0x22。
	f1 := bytes.Repeat([]byte{0x11}, n)
	f2 := bytes.Repeat([]byte{0x22}, n)
	stream := bytes.NewReader(append(append([]byte{}, f1...), f2...))

	c := NewCameraSource(nil)
	done := make(chan struct{})
	go func() { c.readFrames(stream); close(done) }()
	<-done // 读完两帧后结束

	// 第一次 Frame() 应返回最新帧（f2，因为读循环已读完两帧，latest=最后一帧）。
	got := c.Frame()
	if got == nil || got[0] != 0x22 {
		t.Fatalf("应返回最新帧 0x22, 实际 %v", func() any {
			if got == nil {
				return nil
			}
			return got[0]
		}())
	}
	if len(got) != n {
		t.Fatalf("帧长错误: %d", len(got))
	}
	// 无新帧 → nil。
	if c.Frame() != nil {
		t.Fatal("无新帧应返回 nil")
	}
}

// TestFFmpegFilter 验证滤镜链：rotate/mirror 必须真的进 -vf（此前只有 macOS 那条路认，
// ffmpeg 这条路把它们丢了，配了没反应），且顺序为「先旋转、再镜像、最后缩放」。
func TestFFmpegFilter(t *testing.T) {
	cases := []struct {
		name   string
		rotate int
		mirror bool
		want   string
	}{
		{"不转不翻", 0, false, "scale=240:240"},
		{"顺时针90", 90, false, "transpose=1,scale=240:240"},
		{"180", 180, false, "transpose=1,transpose=1,scale=240:240"},
		{"顺时针270", 270, false, "transpose=2,scale=240:240"},
		{"仅镜像", 0, true, "hflip,scale=240:240"},
		{"先转后翻", 90, true, "transpose=1,hflip,scale=240:240"},
		{"负角度归一", -90, false, "transpose=2,scale=240:240"},
		{"非法角度当0", 45, false, "scale=240:240"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ffmpegFilter(c.rotate, c.mirror, 240, 240); got != c.want {
				t.Fatalf("rotate=%d mirror=%v: 期望 %q 实际 %q", c.rotate, c.mirror, c.want, got)
			}
		})
	}
}

// TestCompositorCameraPriority 验证：开启摄像头后优先取摄像头帧。
func TestCompositorCameraPriority(t *testing.T) {
	cam := NewCameraSource(nil)
	// 手动塞一帧到摄像头源。
	f := bytes.Repeat([]byte{0x33}, robot.ImageBytesRGB888)
	cam.readFrames(bytes.NewReader(f)) // 读入一帧后返回

	comp := NewCompositor(cam, NewClipSource(map[string]Clip{}), NewEmotionSource())
	comp.SetCamera(true)
	got := comp.Frame()
	if got == nil || got[0] != 0x33 {
		t.Fatal("开启摄像头后应返回摄像头帧")
	}
}
