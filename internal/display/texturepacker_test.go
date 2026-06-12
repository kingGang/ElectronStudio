package display

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

var red = color.RGBA{255, 0, 0, 255}

// TestTexturePackerArray 验证 JSON(Array) 解析：有序帧、meta.image 图集、非裁剪非旋转。
func TestTexturePackerArray(t *testing.T) {
	dir := t.TempDir()
	// 图集 8×4：左 4×4 帧0(全红), 右 4×4 帧1(全蓝)。
	atlas := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			if x < 4 {
				atlas.Set(x, y, red)
			} else {
				atlas.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	writePNG(t, filepath.Join(dir, "sheet.png"), atlas)
	js := `{"frames":[
	  {"filename":"a","frame":{"x":0,"y":0,"w":4,"h":4},"rotated":false,"trimmed":false,"spriteSourceSize":{"x":0,"y":0,"w":4,"h":4},"sourceSize":{"w":4,"h":4}},
	  {"filename":"b","frame":{"x":4,"y":0,"w":4,"h":4},"rotated":false,"trimmed":false,"spriteSourceSize":{"x":0,"y":0,"w":4,"h":4},"sourceSize":{"w":4,"h":4}}
	],"meta":{"image":"sheet.png","size":{"w":8,"h":4},"fps":12}}`
	_ = os.WriteFile(filepath.Join(dir, "happy.json"), []byte(js), 0o600)

	clips, err := LoadClips(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	clip := clips["happy"]
	if len(clip.Frames) != 2 || clip.FPS != 12 {
		t.Fatalf("帧/帧率错误: %d fps=%d", len(clip.Frames), clip.FPS)
	}
	if clip.Frames[0][0] != 255 || clip.Frames[0][2] != 0 { // 帧0 红
		t.Fatalf("帧0 应为红: %v", clip.Frames[0][:3])
	}
	if clip.Frames[1][2] != 255 { // 帧1 蓝
		t.Fatalf("帧1 应为蓝: %v", clip.Frames[1][:3])
	}
}

// TestReconstructRotated 验证旋转复原：图集中某像素按逆时针映射到正立位置。
func TestReconstructRotated(t *testing.T) {
	// 图集区域 3×2（frame.W=3,frame.H=2）→ 正立 uw=frame.H=2, uh=frame.W=3。
	atlas := image.NewRGBA(image.Rect(0, 0, 3, 2))
	// 在图集本地 (lx,ly)=(0,0) 放红点。
	atlas.Set(0, 0, red)
	fr := tpFrame{
		Frame:            tpRect{X: 0, Y: 0, W: 3, H: 2},
		Rotated:          true,
		SpriteSourceSize: tpRect{X: 0, Y: 0, W: 2, H: 3},
		SourceSize:       tpSize{W: 2, H: 3},
	}
	canvas := reconstructTPFrame(atlas, fr)
	// 公式: atlas(lx,ly)=(dv, H-1-du) → dv=lx=0, du=H-1-ly=2-1-0=1。正立 (du,dv)=(1,0)。
	r, _, _, _ := canvas.At(1, 0).RGBA()
	if byte(r>>8) != 255 {
		t.Fatalf("旋转复原位置错误: 期望 (1,0) 为红, 实际 r=%d", r>>8)
	}
	// 画布尺寸应为 sourceSize 2×3。
	if canvas.Bounds().Dx() != 2 || canvas.Bounds().Dy() != 3 {
		t.Fatalf("画布尺寸错误: %v", canvas.Bounds())
	}
}

// TestReconstructTrimmed 验证裁剪复原：内容按 spriteSourceSize 偏移贴回，其余透明(黑)。
func TestReconstructTrimmed(t *testing.T) {
	atlas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			atlas.Set(x, y, red)
		}
	}
	fr := tpFrame{
		Frame:            tpRect{X: 0, Y: 0, W: 2, H: 2},
		Trimmed:          true,
		SpriteSourceSize: tpRect{X: 1, Y: 1, W: 2, H: 2}, // 内容偏移到 (1,1)
		SourceSize:       tpSize{W: 4, H: 4},
	}
	canvas := reconstructTPFrame(atlas, fr)
	// (0,0) 应透明(alpha 0)；(1,1) 应红。
	if _, _, _, a := canvas.At(0, 0).RGBA(); a != 0 {
		t.Fatal("画布左上应透明")
	}
	if r, _, _, a := canvas.At(1, 1).RGBA(); byte(r>>8) != 255 || a == 0 {
		t.Fatal("内容应贴到 (1,1)")
	}
}

// TestNaturalSort 验证 Hash 形式按文件名自然排序（frame2 在 frame10 之前）。
func TestNaturalSort(t *testing.T) {
	cases := [][2]string{{"frame2", "frame10"}, {"a9", "a10"}, {"0002", "0010"}, {"f1", "f2"}}
	for _, c := range cases {
		if !naturalLess(c[0], c[1]) {
			t.Fatalf("naturalLess(%q,%q) 应为 true", c[0], c[1])
		}
		if naturalLess(c[1], c[0]) {
			t.Fatalf("naturalLess(%q,%q) 应为 false", c[1], c[0])
		}
	}
}

// TestParseFramesHashOrder 验证 Hash 形式解析后按自然序排帧。
func TestParseFramesHashOrder(t *testing.T) {
	raw := `{"f10":{"frame":{"x":0,"y":0,"w":1,"h":1}},"f2":{"frame":{"x":0,"y":0,"w":1,"h":1}},"f1":{"frame":{"x":0,"y":0,"w":1,"h":1}}}`
	frames, err := parseTPFrames([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := []string{frames[0].Filename, frames[1].Filename, frames[2].Filename}
	if got[0] != "f1" || got[1] != "f2" || got[2] != "f10" {
		t.Fatalf("帧序错误: %v", got)
	}
}

// TestParseFramesEmptyArray 验证空数组报"为空"而非"既非数组也非对象"。
func TestParseFramesEmptyArray(t *testing.T) {
	_, err := parseTPFrames([]byte(`[]`))
	if err == nil || !contains(err.Error(), "为空") {
		t.Fatalf("空数组应报'为空', 实际: %v", err)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
