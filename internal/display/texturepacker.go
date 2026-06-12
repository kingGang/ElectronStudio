package display

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 本文件解析 TexturePacker 的 "JSON (Array)"（也兼容 Hash）导出格式，把图集切成
// 正立的整帧序列。正确处理两件易错的事：
//   - rotated：TexturePacker 默认把精灵顺时针 90° 打包；复原需逆时针 90°。
//   - trimmed：去掉了透明边；按 spriteSourceSize 偏移贴回 sourceSize 画布。

type tpRect struct {
	X, Y, W, H int
}
type tpSize struct {
	W, H int
}

// tpFrame 是 frames 数组的一个元素。
type tpFrame struct {
	Filename         string `json:"filename"`
	Frame            tpRect `json:"frame"`            // 图集上(已裁剪、若旋转则为图集中实际宽高)区域
	Rotated          bool   `json:"rotated"`          // true=打包时顺时针旋转 90°
	Trimmed          bool   `json:"trimmed"`          // true=去过透明边
	SpriteSourceSize tpRect `json:"spriteSourceSize"` // 内容在原画布中的包围盒(正立坐标)
	SourceSize       tpSize `json:"sourceSize"`       // 原始未裁剪画布尺寸
}

type tpMeta struct {
	Image string `json:"image"` // 图集 PNG 文件名
	Size  tpSize `json:"size"`
	FPS   int    `json:"fps"` // 非标准；允许用户在 meta 里加
}

type tpSheet struct {
	Frames json.RawMessage `json:"frames"`
	Meta   tpMeta          `json:"meta"`
	FPS    int             `json:"fps"` // 非标准顶层 fps
}

// loadTexturePacker 解析 TexturePacker JSON 并切出帧序列（每帧缩放到 240×240 RGB888）。
func loadTexturePacker(jsonPath, dir string, data []byte) (Clip, error) {
	var sheet tpSheet
	if err := json.Unmarshal(data, &sheet); err != nil {
		return Clip{}, fmt.Errorf("display: 解析 TexturePacker JSON 失败: %w", err)
	}
	frames, err := parseTPFrames(sheet.Frames)
	if err != nil {
		return Clip{}, err
	}

	// 图集 PNG：优先 meta.image，否则回退到同名 <base>.png。
	atlasPath := ""
	if sheet.Meta.Image != "" {
		atlasPath = filepath.Join(dir, sheet.Meta.Image)
	}
	if atlasPath == "" || !fileExists(atlasPath) {
		atlasPath = strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".png"
	}
	img, err := decodeImage(atlasPath)
	if err != nil {
		return Clip{}, fmt.Errorf("display: 加载图集 %s 失败: %w", atlasPath, err)
	}

	out := make([][]byte, 0, len(frames))
	for _, fr := range frames {
		canvas := reconstructTPFrame(img, fr)
		b := canvas.Bounds()
		out = append(out, sampleRGB888(canvas, b.Min.X, b.Min.Y, b.Dx(), b.Dy()))
	}

	fps := sheet.FPS
	if fps <= 0 {
		fps = sheet.Meta.FPS
	}
	if fps <= 0 {
		fps = clipFPS
	}
	return Clip{Frames: out, FPS: fps}, nil
}

// parseTPFrames 解析 frames：数组（JSON-Array，保序）或对象（JSON-Hash，按文件名自然排序）。
// 按首个非空字符判定形态，避免空数组被误判。
func parseTPFrames(raw json.RawMessage) ([]tpFrame, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("display: TexturePacker JSON 缺少 frames")
	}
	switch trimmed[0] {
	case '[':
		var arr []tpFrame
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("display: 解析 frames 数组失败: %w", err)
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("display: frames 数组为空")
		}
		return arr, nil
	case '{':
		var m map[string]tpFrame
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, fmt.Errorf("display: 解析 frames 对象失败: %w", err)
		}
		if len(m) == 0 {
			return nil, fmt.Errorf("display: frames 对象为空")
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		// 自然排序：使 frame2 排在 frame10 之前（字典序会反）。
		sort.Slice(keys, func(i, j int) bool { return naturalLess(keys[i], keys[j]) })
		out := make([]tpFrame, 0, len(m))
		for _, k := range keys {
			f := m[k]
			if f.Filename == "" {
				f.Filename = k
			}
			out = append(out, f)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("display: frames 既非数组也非对象")
	}
}

// naturalLess 自然序比较：把连续数字段按数值比较，其余按字符比较。
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ad, bd := isDigit(a[i]), isDigit(b[j])
		if ad && bd {
			// 取两侧数字段。
			si, sj := i, j
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, nb := trimZeros(a[si:i]), trimZeros(b[sj:j])
			if len(na) != len(nb) {
				return len(na) < len(nb) // 位数少的数值小
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func trimZeros(s string) string {
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}

// reconstructTPFrame 把一个 TexturePacker 帧复原为正立的 sourceSize 整帧（RGBA，透明边保留）。
func reconstructTPFrame(src image.Image, fr tpFrame) *image.RGBA {
	// 正立内容尺寸：旋转时与图集区域宽高互换。
	uw, uh := fr.Frame.W, fr.Frame.H
	if fr.Rotated {
		uw, uh = fr.Frame.H, fr.Frame.W
	}

	// 画布尺寸与内容偏移：优先 sourceSize + spriteSourceSize；
	// sourceSize 缺失时退化为内容尺寸，并把偏移归零（否则会越界错位/截断）。
	sw, sh := fr.SourceSize.W, fr.SourceSize.H
	ox, oy := fr.SpriteSourceSize.X, fr.SpriteSourceSize.Y
	if sw <= 0 || sh <= 0 {
		sw, sh = uw, uh
		ox, oy = 0, 0
	}
	canvas := image.NewRGBA(image.Rect(0, 0, sw, sh)) // 默认全透明

	for du := 0; du < uw; du++ {
		for dv := 0; dv < uh; dv++ {
			var sx, sy int
			if fr.Rotated {
				// 逆时针 90° 复原顺时针打包：up[du][dv] = atlas[X+dv, Y+H-1-du]
				sx = fr.Frame.X + dv
				sy = fr.Frame.Y + fr.Frame.H - 1 - du
			} else {
				sx = fr.Frame.X + du
				sy = fr.Frame.Y + dv
			}
			cx, cy := ox+du, oy+dv
			if cx >= 0 && cy >= 0 && cx < sw && cy < sh {
				canvas.Set(cx, cy, src.At(sx, sy))
			}
		}
	}
	return canvas
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
