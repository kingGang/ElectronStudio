package display

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // 注册 JPEG 解码
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// clipFPS 默认播放帧率（无显式 fps 时）。
const clipFPS = 15

// driverFrameRate 是设备驱动的帧率（device.Driver 固定 30fps），用于换算播放节奏。
const driverFrameRate = 30

// Clip 是一段表情动画：若干帧 + 播放帧率。
type Clip struct {
	Frames [][]byte // 每帧 240×240×3 RGB888
	FPS    int      // 播放帧率
}

// ClipSource 播放离线表情动画（每情绪一段，循环）。
// 帧来源支持两种：① 目录 emotions/<情绪>/*.png（一帧一文件）；
// ② 精灵图集 emotions/<情绪>.png + emotions/<情绪>.json（一张大图切帧）。
type ClipSource struct {
	mu      sync.Mutex
	emotion string
	clips   map[string]Clip
	idx     int
	tick    int
}

// NewClipSource 用已加载的动画集创建播放源。
func NewClipSource(clips map[string]Clip) *ClipSource {
	if clips == nil {
		clips = map[string]Clip{}
	}
	return &ClipSource{emotion: "neutral", clips: clips}
}

// Has 报告某情绪是否有可播动画。
func (c *ClipSource) Has(emotion string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.clips[emotion].Frames) > 0
}

// SetEmotion 切换情绪并重置到首帧。
func (c *ClipSource) SetEmotion(e string) {
	c.mu.Lock()
	if e != c.emotion {
		c.emotion = e
		c.idx = 0
		c.tick = 0
	}
	c.mu.Unlock()
}

// Frame 实现 Source：按该片帧率推进并返回当前帧；无动画返回 nil。
func (c *ClipSource) Frame() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	clip := c.clips[c.emotion]
	if len(clip.Frames) == 0 {
		return nil
	}
	fps := clip.FPS
	if fps <= 0 {
		fps = clipFPS
	}
	everyN := driverFrameRate / fps
	if everyN < 1 {
		everyN = 1
	}
	c.tick++
	if c.tick%everyN == 0 {
		c.idx = (c.idx + 1) % len(clip.Frames)
	}
	f := clip.Frames[c.idx%len(clip.Frames)]
	out := make([]byte, len(f))
	copy(out, f)
	return out
}

// LoadClips 从 dir 加载各情绪动画，三种来源自动识别：
//   - 子目录 <情绪>/*.{png,jpg}             → 一帧一文件
//   - <情绪>.json (TexturePacker JSON-Array) → 图集 + meta.image，处理 rotated/trimmed
//   - <情绪>.json (自带网格 meta frame_width) → 网格切帧
//
// 目录不存在时返回空集（不报错），调用方据此回退到程序动画脸。
func LoadClips(dir string) (map[string]Clip, error) {
	out := make(map[string]Clip)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("display: 读取素材目录失败: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if frames, err := loadEmotionFrames(filepath.Join(dir, name)); err == nil && len(frames) > 0 {
				out[name] = Clip{Frames: frames, FPS: clipFPS}
			}
			continue
		}
		// 以 .json 为入口（情绪名=json 文件名）。
		if strings.EqualFold(filepath.Ext(name), ".json") {
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if clip, err := loadAtlas(filepath.Join(dir, name), dir); err == nil && len(clip.Frames) > 0 {
				out[base] = clip
			}
		}
	}
	return out, nil
}

// loadAtlas 读取图集 meta 并自动选择格式：TexturePacker（含 frames）或网格（含 frame_width）。
func loadAtlas(jsonPath, dir string) (Clip, error) {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return Clip{}, err
	}
	var probe struct {
		FrameWidth int             `json:"frame_width"`
		Frames     json.RawMessage `json:"frames"`
	}
	_ = json.Unmarshal(data, &probe)
	switch {
	case probe.FrameWidth > 0:
		png := strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".png"
		return loadGridSheet(png, data)
	case len(probe.Frames) > 0:
		return loadTexturePacker(jsonPath, dir, data)
	default:
		return Clip{}, fmt.Errorf("display: 未识别的图集 meta %s", jsonPath)
	}
}

// sheetMeta 描述（本项目自有的）网格切帧规则。
type sheetMeta struct {
	FrameWidth  int `json:"frame_width"`
	FrameHeight int `json:"frame_height"`
	Frames      int `json:"frames"` // 帧数；<=0 时取满网格
	FPS         int `json:"fps"`
}

// loadGridSheet 把一张图集按等距网格切成帧序列（每帧缩放到 240×240 RGB888）。
func loadGridSheet(pngPath string, metaData []byte) (Clip, error) {
	var m sheetMeta
	if err := json.Unmarshal(metaData, &m); err != nil {
		return Clip{}, err
	}
	if m.FrameWidth <= 0 || m.FrameHeight <= 0 {
		return Clip{}, fmt.Errorf("display: 网格 meta 缺少 frame_width/height")
	}
	img, err := decodeImage(pngPath)
	if err != nil {
		return Clip{}, err
	}
	b := img.Bounds()
	cols := b.Dx() / m.FrameWidth
	rows := b.Dy() / m.FrameHeight
	if cols < 1 || rows < 1 {
		return Clip{}, fmt.Errorf("display: 精灵图集尺寸与帧大小不符")
	}
	n := m.Frames
	if n <= 0 || n > cols*rows {
		n = cols * rows
	}
	frames := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		sx := b.Min.X + (i%cols)*m.FrameWidth
		sy := b.Min.Y + (i/cols)*m.FrameHeight
		frames = append(frames, sampleRGB888(img, sx, sy, m.FrameWidth, m.FrameHeight))
	}
	return Clip{Frames: frames, FPS: m.FPS}, nil
}

// loadEmotionFrames 读取一个情绪目录下的全部帧图（按名排序）并转为 240×240 RGB888。
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
		img, err := decodeImage(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		b := img.Bounds()
		frames = append(frames, sampleRGB888(img, b.Min.X, b.Min.Y, b.Dx(), b.Dy()))
	}
	return frames, nil
}

// decodeImage 解码一张图片（png/jpg）。
func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		_, _ = f.Seek(0, 0)
		return png.Decode(f)
	}
	return img, nil
}

// sampleRGB888 从 img 的 (sx,sy,fw,fh) 区域以最近邻缩放到 240×240，输出 RGB888。
// 一次完成"裁剪 + 缩放 + 转格式"，适配任意帧尺寸。
func sampleRGB888(img image.Image, sx, sy, fw, fh int) []byte {
	out := make([]byte, robot.ImageBytesRGB888)
	for y := 0; y < scrH; y++ {
		srcY := sy + y*fh/scrH
		for x := 0; x < scrW; x++ {
			srcX := sx + x*fw/scrW
			r, g, b, _ := img.At(srcX, srcY).RGBA()
			i := (y*scrW + x) * 3
			out[i] = byte(r >> 8)
			out[i+1] = byte(g >> 8)
			out[i+2] = byte(b >> 8)
		}
	}
	return out
}
