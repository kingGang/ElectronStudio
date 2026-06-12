package display

import (
	"fmt"
	"image"
	_ "image/jpeg" // 允许解码 JPEG 素材
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// ClipSource 播放【离线（AI）生成的表情帧序列】：每个情绪一段循环动画。
// 帧素材为 240×240 的 PNG/JPG，放在 <dir>/<emotion>/*.png；运行时循环播放。
// 没有对应素材的情绪返回 nil（由 Compositor 回退到程序动画脸）。
type ClipSource struct {
	mu       sync.Mutex
	emotion  string
	clips    map[string][][]byte // 情绪 -> 帧序列（每帧 240×240×3 RGB888）
	idx      int
	tick     int
	everyN   int // 每 everyN 个驱动帧前进一帧（驱动 30fps，everyN=2 → 15fps 播放）
}

// NewClipSource 用已加载的帧集创建播放源。
func NewClipSource(clips map[string][][]byte) *ClipSource {
	return &ClipSource{emotion: "neutral", clips: clips, everyN: 2}
}

// Has 报告某情绪是否有可播素材。
func (c *ClipSource) Has(emotion string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.clips[emotion]) > 0
}

// SetEmotion 切换情绪并重置到该片首帧。
func (c *ClipSource) SetEmotion(e string) {
	c.mu.Lock()
	if e != c.emotion {
		c.emotion = e
		c.idx = 0
	}
	c.mu.Unlock()
}

// Frame 实现 Source：按播放帧率推进，返回当前帧；无素材返回 nil。
func (c *ClipSource) Frame() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	frames := c.clips[c.emotion]
	if len(frames) == 0 {
		return nil
	}
	c.tick++
	if c.everyN > 0 && c.tick%c.everyN == 0 {
		c.idx = (c.idx + 1) % len(frames)
	}
	f := frames[c.idx%len(frames)]
	out := make([]byte, len(f))
	copy(out, f)
	return out
}

// LoadClips 从 dir 加载各情绪的帧素材：dir/<emotion>/*.{png,jpg}（按文件名排序为帧序）。
// 目录不存在时返回空集（不报错），调用方据此回退到程序动画脸。
func LoadClips(dir string) (map[string][][]byte, error) {
	out := make(map[string][][]byte)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("display: 读取素材目录失败: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		frames, err := loadEmotionFrames(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if len(frames) > 0 {
			out[e.Name()] = frames
		}
	}
	return out, nil
}

// loadEmotionFrames 读取一个情绪目录下的全部帧图（按名排序）并转为 RGB888。
func loadEmotionFrames(dir string) ([][]byte, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if !f.IsDir() {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)

	var frames [][]byte
	for _, name := range names {
		rgb, err := decodeRGB888(filepath.Join(dir, name))
		if err != nil {
			continue // 跳过无法解码的文件（如 .gitkeep）
		}
		frames = append(frames, rgb)
	}
	return frames, nil
}

// decodeRGB888 解码一张图片为 240×240 RGB888（要求图片本身即 240×240）。
func decodeRGB888(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		// 退回 png 显式解码（image.Decode 需要已注册解码器）。
		_, _ = f.Seek(0, 0)
		img, err = png.Decode(f)
		if err != nil {
			return nil, err
		}
	}
	b := img.Bounds()
	if b.Dx() != scrW || b.Dy() != scrH {
		return nil, fmt.Errorf("display: 素材尺寸须为 %dx%d, 实际 %dx%d", scrW, scrH, b.Dx(), b.Dy())
	}
	out := make([]byte, robot.ImageBytesRGB888)
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA() // 16-bit, 取高 8 位
			out[i] = byte(r >> 8)
			out[i+1] = byte(g >> 8)
			out[i+2] = byte(bl >> 8)
			i += 3
		}
	}
	return out, nil
}
