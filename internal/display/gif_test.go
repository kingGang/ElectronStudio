package display

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"testing"
)

// writeGIF 把构造好的 gif.GIF 编码落盘，便于驱动 loadGIF 走真实解码路径。
func writeGIF(t *testing.T, path string, g *gif.GIF) {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("编码 GIF 失败: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("写 GIF 失败: %v", err)
	}
}

// pixAt 取一帧 240×240 RGB888 中某像素的 RGB。
func pixAt(frame []byte, x, y int) (byte, byte, byte) {
	i := (y*scrW + x) * 3
	return frame[i], frame[i+1], frame[i+2]
}

// TestLoadGIFBasic 验证两整帧（红→蓝）：帧数、颜色、由延时换算的 fps。
func TestLoadGIFBasic(t *testing.T) {
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	f0 := image.NewPaletted(image.Rect(0, 0, 4, 4), pal) // 全红(索引0)
	f1 := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
	for i := range f1.Pix {
		f1.Pix[i] = 1 // 全蓝(索引1)
	}
	g := &gif.GIF{Image: []*image.Paletted{f0, f1}, Delay: []int{10, 10}} // 10cs/帧 → 10fps

	p := filepath.Join(t.TempDir(), "happy.gif")
	writeGIF(t, p, g)

	clip, err := loadGIF(p)
	if err != nil {
		t.Fatalf("loadGIF: %v", err)
	}
	if len(clip.Frames) != 2 {
		t.Fatalf("帧数应为 2, 实际 %d", len(clip.Frames))
	}
	if clip.FPS != 10 {
		t.Fatalf("fps 应为 10, 实际 %d", clip.FPS)
	}
	if r, _, b := pixAt(clip.Frames[0], 0, 0); r != 255 || b != 0 {
		t.Fatalf("帧0 应为红, 实际 r=%d b=%d", r, b)
	}
	if _, _, b := pixAt(clip.Frames[1], 0, 0); b != 255 {
		t.Fatalf("帧1 应为蓝, 实际 b=%d", b)
	}
}

// TestLoadGIFDisposalBackground 验证「还原背景」处置 + 子帧透明叠加：
// 帧1 只画左上一个绿点（其余透明），背景被清后应只剩绿点，其余为黑。
// 同时验证全 0 延时回退默认 fps。
func TestLoadGIFDisposalBackground(t *testing.T) {
	pal := color.Palette{
		color.RGBA{255, 0, 0, 255}, // 0 红
		color.RGBA{0, 255, 0, 255}, // 1 绿
		color.RGBA{0, 0, 0, 0},     // 2 透明
	}
	full := image.Rect(0, 0, 2, 2)
	f0 := image.NewPaletted(full, pal) // 全红
	f1 := image.NewPaletted(full, pal)
	for i := range f1.Pix {
		f1.Pix[i] = 2 // 全透明
	}
	f1.SetColorIndex(0, 0, 1) // 左上绿

	g := &gif.GIF{
		Image:    []*image.Paletted{f0, f1},
		Delay:    []int{0, 0},
		Disposal: []byte{gif.DisposalBackground, gif.DisposalNone},
	}
	p := filepath.Join(t.TempDir(), "x.gif")
	writeGIF(t, p, g)

	clip, err := loadGIF(p)
	if err != nil {
		t.Fatalf("loadGIF: %v", err)
	}
	if len(clip.Frames) != 2 {
		t.Fatalf("帧数应为 2, 实际 %d", len(clip.Frames))
	}
	if r, _, _ := pixAt(clip.Frames[0], scrW/2, scrH/2); r != 255 {
		t.Fatalf("帧0 中心应红, r=%d", r)
	}
	if _, gg, _ := pixAt(clip.Frames[1], 0, 0); gg != 255 {
		t.Fatalf("帧1 左上应绿, g=%d", gg)
	}
	if r, gg, b := pixAt(clip.Frames[1], scrW-1, scrH-1); r != 0 || gg != 0 || b != 0 {
		t.Fatalf("帧1 右下应为黑(背景已清), 实际 %d,%d,%d", r, gg, b)
	}
	if clip.FPS != clipFPS {
		t.Fatalf("全 0 延时应回退默认 fps=%d, 实际 %d", clipFPS, clip.FPS)
	}
}

// TestValidateUpload 验证上传校验：合法 GIF/图片通过；超帧数、垃圾字节、不支持类型被拒。
func TestValidateUpload(t *testing.T) {
	pal := color.Palette{color.RGBA{1, 2, 3, 255}}

	// 合法小 GIF。
	okGif := &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 2, 2), pal)}, Delay: []int{5}}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, okGif); err != nil {
		t.Fatal(err)
	}
	if n, err := ValidateUpload(".gif", b.Bytes()); err != nil || n != 1 {
		t.Fatalf("合法 GIF 应通过, n=%d err=%v", n, err)
	}

	// 超过帧数上限 → 拒绝。
	var frames []*image.Paletted
	var delays []int
	for i := 0; i < maxGIFFrames+1; i++ {
		frames = append(frames, image.NewPaletted(image.Rect(0, 0, 1, 1), pal))
		delays = append(delays, 5)
	}
	var b2 bytes.Buffer
	if err := gif.EncodeAll(&b2, &gif.GIF{Image: frames, Delay: delays}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateUpload(".gif", b2.Bytes()); err == nil {
		t.Fatalf("超过帧数上限(%d)应被拒绝", maxGIFFrames)
	}

	// 垃圾字节 / 不支持类型 → 拒绝。
	if _, err := ValidateUpload(".gif", []byte("not a gif")); err == nil {
		t.Fatal("非 GIF 字节应被拒绝")
	}
	if _, err := ValidateUpload(".bmp", []byte("x")); err == nil {
		t.Fatal("不支持的类型应被拒绝")
	}
}

// TestClipFrameRateAccumulator 验证浮点累加器：20fps 源在 ~1 秒(30 个 30fps tick)内
// 推进约 20 帧，而非旧整数 everyN 实现的 30 帧（回归守卫）。
func TestClipFrameRateAccumulator(t *testing.T) {
	n := 60
	frames := make([][]byte, n)
	for i := range frames {
		f := make([]byte, scrW*scrH*3)
		f[0] = byte(i) // 每帧首字节=帧号，便于观察推进
		frames[i] = f
	}
	cs := NewClipSource(map[string]Clip{"e": {Frames: frames, FPS: 20}})
	cs.SetEmotion("e")

	advances := 0
	last := cs.Frame()[0]
	for i := 0; i < 30; i++ { // 30 tick ≈ 1s @30fps
		cur := cs.Frame()[0]
		if cur != last {
			advances++
			last = cur
		}
	}
	if advances < 18 || advances > 22 {
		t.Fatalf("20fps 在 ~1s 内应推进约 20 次, 实际 %d（旧整数实现会得到 30）", advances)
	}
}

// TestLoadEmotionDirFPS 验证目录素材读取可选 clip.json 帧率（视频抽帧后即此形式），
// 且 clip.json 本身不被当作帧。
func TestLoadEmotionDirFPS(t *testing.T) {
	dir := t.TempDir()
	emo := filepath.Join(dir, "wave")
	if err := os.MkdirAll(emo, 0o755); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	writePNG(t, filepath.Join(emo, "0001.png"), img)
	writePNG(t, filepath.Join(emo, "0002.png"), img)
	if err := os.WriteFile(filepath.Join(emo, "clip.json"), []byte(`{"fps":24}`), 0o600); err != nil {
		t.Fatal(err)
	}

	clips, err := LoadClips(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := clips["wave"]
	if len(c.Frames) != 2 {
		t.Fatalf("帧数应为 2（clip.json 不算帧），实际 %d", len(c.Frames))
	}
	if c.FPS != 24 {
		t.Fatalf("应读取 clip.json fps=24, 实际 %d", c.FPS)
	}
}

// TestReplaceAndInfo 验证热重载替换与素材概要/首帧读取。
func TestReplaceAndInfo(t *testing.T) {
	frame := make([]byte, scrW*scrH*3)
	frame[0] = 200
	cs := NewClipSource(map[string]Clip{"happy": {Frames: [][]byte{frame}, FPS: 12}})

	info := cs.Info()
	if len(info) != 1 || info[0].Name != "happy" || info[0].Frames != 1 || info[0].FPS != 12 {
		t.Fatalf("Info 概要错误: %+v", info)
	}
	if ff := cs.FirstFrame("happy"); ff == nil || ff[0] != 200 {
		t.Fatalf("FirstFrame 应返回首帧副本")
	}
	// 修改返回的副本不应影响内部数据。
	cs.FirstFrame("happy")[0] = 9
	if ff := cs.FirstFrame("happy"); ff[0] != 200 {
		t.Fatalf("FirstFrame 应返回副本, 内部被改写")
	}

	cs.Replace(map[string]Clip{"sad": {Frames: [][]byte{frame}, FPS: 8}})
	if cs.Has("happy") || !cs.Has("sad") {
		t.Fatalf("Replace 后应只剩 sad")
	}
}
