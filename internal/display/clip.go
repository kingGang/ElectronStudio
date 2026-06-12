package display

import (
	"bytes"
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
	acc     float64 // 帧推进累加器：每个 30fps tick += fps/30，>=1 时推进一帧（精确支持任意帧率）
}

// NewClipSource 用已加载的动画集创建播放源。
func NewClipSource(clips map[string]Clip) *ClipSource {
	if clips == nil {
		clips = map[string]Clip{}
	}
	return &ClipSource{emotion: "neutral", clips: clips}
}

// ClipInfo 描述一段已加载的表情素材（供素材管理界面展示）。
type ClipInfo struct {
	Name   string // 情绪名
	Frames int    // 帧数
	FPS    int    // 播放帧率
}

// Replace 原子替换全部动画集（素材增删后热重载，无需重启进程）。
// 保持当前情绪，但把播放进度重置到首帧，避免索引错位与跳帧。
func (c *ClipSource) Replace(clips map[string]Clip) {
	if clips == nil {
		clips = map[string]Clip{}
	}
	c.mu.Lock()
	c.clips = clips
	c.idx = 0
	c.acc = 0
	c.mu.Unlock()
}

// ValidateUpload 在落盘前校验一段上传的素材字节能否解析为有效动画，并施加尺寸/帧数上限。
// 这同时实现两个目的：① 防「解码炸弹」（小文件声明超大画布/超多帧耗尽内存）；
// ② 让“写盘”只发生在确认有效之后，避免坏文件覆盖删除既有有效素材造成数据丢失。
// ext 为小写扩展名（.gif/.png/.jpg/.jpeg）；通过则返回帧数，否则返回 error。
func ValidateUpload(ext string, data []byte) (int, error) {
	switch ext {
	case ".gif":
		clip, err := decodeGIFClip(data)
		if err != nil {
			return 0, err
		}
		if len(clip.Frames) == 0 {
			return 0, fmt.Errorf("display: GIF 无有效帧")
		}
		return len(clip.Frames), nil
	case ".png", ".jpg", ".jpeg":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, fmt.Errorf("display: 无法识别图片格式: %w", err)
		}
		if cfg.Width > maxImageDim || cfg.Height > maxImageDim {
			return 0, fmt.Errorf("display: 图片尺寸过大 %dx%d（上限 %d）", cfg.Width, cfg.Height, maxImageDim)
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return 0, fmt.Errorf("display: 图片解码失败: %w", err)
		}
		return 1, nil
	default:
		return 0, fmt.Errorf("display: 不支持的文件类型 %q（支持 .gif / .png / .jpg）", ext)
	}
}

// Info 返回所有已加载素材的概要（名称/帧数/帧率），按名称排序，供 UI 列表展示。
func (c *ClipSource) Info() []ClipInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ClipInfo, 0, len(c.clips))
	for name, clip := range c.clips {
		fps := clip.FPS
		if fps <= 0 {
			fps = clipFPS
		}
		out = append(out, ClipInfo{Name: name, Frames: len(clip.Frames), FPS: fps})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FirstFrame 返回某情绪动画的首帧副本（240×240 RGB888），供 UI 生成缩略图；无则返回 nil。
func (c *ClipSource) FirstFrame(emotion string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	clip := c.clips[emotion]
	if len(clip.Frames) == 0 {
		return nil
	}
	f := clip.Frames[0]
	out := make([]byte, len(f))
	copy(out, f)
	return out
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
		c.acc = 0
	}
	c.mu.Unlock()
}

// Frame 实现 Source：按当前情绪与该片帧率推进并返回当前帧；无动画返回 nil。
func (c *ClipSource) Frame() []byte {
	c.mu.Lock()
	e := c.emotion
	c.mu.Unlock()
	return c.FrameFor(e)
}

// FrameFor 原子地（单次加锁内）按指定情绪推进并返回当前帧；该情绪无动画时返回 nil。
// 供 Compositor 调用，避免 Has()+Frame() 两次加锁之间的竞态（情绪切换/热重载瞬间取错帧）。
//
// 帧推进用浮点累加器而非整数“每隔 N tick”：驱动固定 30fps，每次调用 acc += fps/30，
// acc>=1 时推进一帧并扣减。这样任意源帧率（如 20/25fps）都能按真实速率播放，
// 不会被整数除法截断成 30fps（曾导致动画明显比原图快）。
func (c *ClipSource) FrameFor(emotion string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if emotion != c.emotion {
		c.emotion = emotion
		c.idx = 0
		c.acc = 0
	}
	clip := c.clips[c.emotion]
	if len(clip.Frames) == 0 {
		return nil
	}
	fps := clip.FPS
	if fps <= 0 {
		fps = clipFPS
	}
	c.acc += float64(fps) / driverFrameRate
	for c.acc >= 1 {
		c.idx = (c.idx + 1) % len(clip.Frames)
		c.acc--
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
			if clip, err := loadEmotionDir(filepath.Join(dir, name)); err == nil && len(clip.Frames) > 0 {
				out[name] = clip
			}
			continue
		}
		// GIF：纯 Go 解码为动画片（情绪名=GIF 文件名），帧率取自 GIF 延时。
		// 这是素材管理界面上传 GIF 后的默认落盘形式，无需任何外部工具。
		if strings.EqualFold(filepath.Ext(name), ".gif") {
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if clip, err := loadGIF(filepath.Join(dir, name)); err == nil && len(clip.Frames) > 0 {
				out[base] = clip
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

// loadEmotionDir 加载一个情绪目录（一帧一文件）为动画片，并读取可选的 clip.json 帧率。
// 视频上传经 ffmpeg 抽帧后即以此形式落盘（含 clip.json 记录 fps）；无 clip.json 时回退默认帧率。
func loadEmotionDir(dir string) (Clip, error) {
	frames, err := loadEmotionFrames(dir)
	if err != nil {
		return Clip{}, err
	}
	fps := clipFPS
	if data, err := os.ReadFile(filepath.Join(dir, "clip.json")); err == nil {
		var m struct {
			FPS int `json:"fps"`
		}
		if json.Unmarshal(data, &m) == nil && m.FPS > 0 {
			fps = m.FPS
		}
	}
	return Clip{Frames: frames, FPS: fps}, nil
}

// loadEmotionFrames 读取一个情绪目录下的全部帧图（按名排序）并转为 240×240 RGB888。
// 非图片文件（如 clip.json）解码失败会被自动跳过，不计入帧。
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
	// 先按图片头校验尺寸上限（防解码炸弹），再整张解码。
	if cfg, _, cfgErr := image.DecodeConfig(f); cfgErr == nil {
		if cfg.Width > maxImageDim || cfg.Height > maxImageDim {
			return nil, fmt.Errorf("display: 图片尺寸过大 %dx%d（上限 %d）", cfg.Width, cfg.Height, maxImageDim)
		}
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
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
