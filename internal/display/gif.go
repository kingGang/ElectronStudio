package display

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"os"
)

// 解码上限（防「解码炸弹」：极小的文件声明超大逻辑屏或超多帧，解码时耗尽内存）。
const (
	maxImageDim  = 4096 // 图片 / GIF 逻辑屏最大边长（远大于 240 屏，留足余量）
	maxGIFFrames = 600  // 单个 GIF 最大帧数
)

// loadGIF 用纯 Go（标准库 image/gif，无 cgo、无外部工具）把一个 GIF 解码为表情动画片。
//
// 它按 GIF 的逐帧处置方式（disposal）把「只画了变化区域」的帧正确合成回完整画面，
// 再缩放到 240×240 RGB888；帧率取自各帧延时的平均值。
// 这样素材管理界面只需把用户上传的 GIF 落盘为 emotions/<情绪>.gif，启动/热重载即自动成为表情，
// 不再依赖 ffmpeg / TexturePacker 等外部命令。
func loadGIF(path string) (Clip, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Clip{}, err
	}
	clip, err := decodeGIFClip(data)
	if err != nil {
		return Clip{}, fmt.Errorf("%w (%s)", err, path)
	}
	return clip, nil
}

// decodeGIFClip 从内存字节解码 GIF 为动画片，并施加尺寸/帧数上限（防解码炸弹）。
// 上传校验与磁盘加载共用此函数，确保「能上传成功」与「能加载播放」判定一致。
func decodeGIFClip(data []byte) (Clip, error) {
	// 先用 DecodeConfig 拿到逻辑屏尺寸做上限判断，避免对超大画布做整块分配。
	if cfg, err := gif.DecodeConfig(bytes.NewReader(data)); err == nil {
		if cfg.Width > maxImageDim || cfg.Height > maxImageDim {
			return Clip{}, fmt.Errorf("display: GIF 逻辑屏尺寸过大 %dx%d（上限 %d）", cfg.Width, cfg.Height, maxImageDim)
		}
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return Clip{}, fmt.Errorf("display: 解析 GIF 失败: %w", err)
	}
	if len(g.Image) == 0 {
		return Clip{}, fmt.Errorf("display: GIF 无帧")
	}
	if len(g.Image) > maxGIFFrames {
		return Clip{}, fmt.Errorf("display: GIF 帧数过多 %d（上限 %d）", len(g.Image), maxGIFFrames)
	}
	return Clip{Frames: gifFramesRGB888(g), FPS: gifFPS(g)}, nil
}

// gifFramesRGB888 把 GIF 的每一帧合成为整屏画面并缩放成 240×240 RGB888。
//
// 正确处理三种处置方式：
//   - 保留（None / 0 / 1）：本帧叠加后保留，作为下一帧的底；
//   - 还原背景（Background / 2）：绘制并产出本帧后，把本帧矩形清为透明（圆屏背景为黑）；
//   - 还原前一帧（Previous / 3）：绘制前快照画布，产出本帧后再还原回去。
//
// GIF 帧可能只是子矩形且含透明索引，draw.Over 会自动只覆盖不透明像素，从而复原完整画面。
func gifFramesRGB888(g *gif.GIF) [][]byte {
	// 画布尺寸优先用全局逻辑屏尺寸，缺省时退化为首帧边界。
	w, h := g.Config.Width, g.Config.Height
	if w <= 0 || h <= 0 {
		b := g.Image[0].Bounds()
		w, h = b.Dx(), b.Dy()
	}
	bounds := image.Rect(0, 0, w, h)
	canvas := image.NewRGBA(bounds)

	out := make([][]byte, 0, len(g.Image))
	for i, frame := range g.Image {
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}

		// 还原前一帧需要在绘制前快照当前画布。
		var backup []byte
		if disposal == gif.DisposalPrevious {
			backup = make([]byte, len(canvas.Pix))
			copy(backup, canvas.Pix)
		}

		// 把本帧（可能为子矩形、含透明）叠加到画布。
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		// 捕获当前完整画面 → 缩放为 240×240 RGB888。
		out = append(out, sampleRGB888(canvas, 0, 0, w, h))

		// 产出后再按处置方式更新画布，供下一帧叠加。
		switch disposal {
		case gif.DisposalBackground:
			clearRect(canvas, frame.Bounds())
		case gif.DisposalPrevious:
			copy(canvas.Pix, backup)
		}
	}
	return out
}

// clearRect 把画布指定矩形清为透明（用于 GIF 的「还原背景」处置）。
// 圆屏背景为黑，透明像素在 sampleRGB888 取色时即为黑，符合预期。
func clearRect(img *image.RGBA, r image.Rectangle) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0, 0, 0, 0
		}
	}
}

// gifFPS 由各帧平均延时（百分之一秒/帧）换算播放帧率，限制在 [1, driverFrameRate]。
// 延时缺失或全为 0（部分 GIF 不写延时）时回退到默认 clipFPS。
func gifFPS(g *gif.GIF) int {
	total, n := 0, 0
	for _, d := range g.Delay {
		if d > 0 {
			total += d
			n++
		}
	}
	if n == 0 || total == 0 {
		return clipFPS
	}
	avg := float64(total) / float64(n) // 百分之一秒/帧
	fps := int(100.0/avg + 0.5)
	if fps < 1 {
		fps = 1
	}
	if fps > driverFrameRate {
		fps = driverFrameRate
	}
	return fps
}
