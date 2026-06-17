package display

// 内置默认表情：随二进制一起嵌入的一套开箱即用表情 GIF。
//
// 来源是稚晖君 ElectronBot 原仓库（github.com/peng-zhihui/ElectronBot，4.CAD-Model/Emoji/）
// 的官方机器人表情——青色眼睛的圆屏脸，每个情绪的「可循环动作」段转成 240×240 循环 GIF。
// 情绪名映射到本项目/小智的情绪键：兴奋→happy(laughing/funny)、难过→sad(crying)、
// 愤怒→angry、惊恐→surprised(shocked)、不屑→cool(confident)、单次眨眼→neutral、右上看→thinking。
//
// 设计：首次运行时把这套默认表情「播种」到 emotions/ 目录（每个表情一生只播种一次，
// 记录在 emotions/.seeded-defaults），此后它们就是普通的磁盘素材——
// 在「素材」页可见、可预览、可删除，也可上传同名 GIF 覆盖。
// 因为只播种一次，用户删掉某个默认表情后不会在下次启动被重新写回。

import (
	"embed"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed emotions_default/*.gif
var defaultEmotionsFS embed.FS

const seededMarker = ".seeded-defaults"

// defaultEmotionGIFs 返回内置默认表情：情绪名（=去扩展名的文件名）→ GIF 原始字节。
func defaultEmotionGIFs() map[string][]byte {
	out := map[string][]byte{}
	entries, err := defaultEmotionsFS.ReadDir("emotions_default")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".gif") {
			continue
		}
		data, err := defaultEmotionsFS.ReadFile("emotions_default/" + e.Name())
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))] = data
	}
	return out
}

// SeedDefaultEmotions 把内置默认表情写入 dir，作为开箱即用的初始表情，返回本次新写入的数量。
//
// 规则（每个默认表情一生只播种一次）：
//   - dir/.seeded-defaults 里已记录的名字不再播种 —— 从而尊重用户后续的删除；
//   - 当前 dir 已存在同名素材（<情绪>.gif / <情绪>/ 目录 / <情绪>.json）的跳过，不覆盖用户自定义；
//   - 无论本次是否实际写入，名字都会被记入 marker（"已处理过"）。
func SeedDefaultEmotions(dir string) (int, error) {
	defs := defaultEmotionGIFs()
	if len(defs) == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}

	markerPath := filepath.Join(dir, seededMarker)
	seeded := map[string]bool{}
	if b, err := os.ReadFile(markerPath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if s := strings.TrimSpace(line); s != "" {
				seeded[s] = true
			}
		}
	}

	n := 0
	for name, data := range defs {
		if seeded[name] {
			continue
		}
		seeded[name] = true // 记录：今后不再播种该名字（即使本次跳过或之后被删除）
		if emotionExists(dir, name) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name+".gif"), data, 0o644); err != nil {
			return n, err
		}
		n++
	}

	names := make([]string, 0, len(seeded))
	for k := range seeded {
		names = append(names, k)
	}
	sort.Strings(names)
	_ = os.WriteFile(markerPath, []byte(strings.Join(names, "\n")+"\n"), 0o644)
	return n, nil
}

// emotionExists 判断 dir 中是否已有名为 name 的任意形式素材。
func emotionExists(dir, name string) bool {
	if fi, err := os.Stat(filepath.Join(dir, name+".gif")); err == nil && !fi.IsDir() {
		return true
	}
	if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.IsDir() {
		return true
	}
	if fi, err := os.Stat(filepath.Join(dir, name+".json")); err == nil && !fi.IsDir() {
		return true
	}
	return false
}
