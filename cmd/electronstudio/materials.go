package main

// 屏幕表情素材管理：让用户在界面上传/删除「机器人脸」动画素材，无需再跑脚本。
//
// 设计：
//   - 上传走 HTTP（POST /api/materials，multipart），因为是二进制文件（GIF/图片），
//     不适合塞进 1MiB 上限的 WebSocket 文本帧；
//   - 列表/删除走 WebSocket（materials 事件 / material_delete 命令），与其余 UI 状态一致；
//   - 缩略图走 HTTP（GET /api/material-thumb?name=...），返回首帧 PNG。
//
// 落盘形式（与 display.LoadClips 的识别规则对齐）：
//   - GIF        → emotions/<情绪>.gif（纯 Go 解码，无外部工具）
//   - 静态图片    → emotions/<情绪>/0001.<ext>（目录=一帧一文件）
//
// 上传/删除后调用 reloadClips 热重载，立即生效并镜像到设备屏。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/robot"
)

const (
	maxUploadBytes = 64 << 20          // 单次素材上传大小上限（视频偏大，放宽到 64 MiB）
	videoFPS       = 20                // 视频抽帧的目标帧率（兼顾流畅与体积；上限 maxGIFFrames 帧）
	videoTimeout   = 90 * time.Second  // ffmpeg 抽帧超时，防异常输入卡死
)

// videoExts 是支持的视频扩展名（经 ffmpeg 抽帧）。
var videoExts = map[string]bool{
	".mp4": true, ".webm": true, ".mov": true, ".mkv": true, ".avi": true, ".m4v": true,
}

// materialRoutes 在 HTTP mux 上挂载素材管理的 REST 接口。
func (a *app) materialRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/materials", a.handleMaterialUpload)
	mux.HandleFunc("/api/material-thumb", a.handleMaterialThumb)
}

// handleMaterialUpload 接收 multipart 上传（字段 name + 文件 file），落盘并热重载。
func (a *app) handleMaterialUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, "上传过大或格式错误: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, ok := sanitizeMaterialName(r.FormValue("name"))
	if !ok {
		http.Error(w, "情绪名非法（仅允许字母/数字/下划线/连字符，≤40 字符）", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少文件字段 file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(hdr.Filename))

	// 视频走 ffmpeg 抽帧（无 cgo，与摄像头同一依赖），其余（GIF/图片）走纯 Go 内存校验。
	if videoExts[ext] {
		a.handleVideoUpload(w, r, name, ext, file)
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "读取上传内容失败", http.StatusBadRequest)
		return
	}

	// 落盘前先在内存校验：能否解析为有效动画 + 尺寸/帧数上限。
	// 双重作用：① 挡住「解码炸弹」；② 只有确认有效后才动磁盘——避免坏文件覆盖删除既有素材。
	if _, err := display.ValidateUpload(ext, data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest) // 校验类错误对用户有意义，可回显
		return
	}

	// 仅“替换文件 + 重载”需要串行化（重的解码已在锁外完成）。
	a.matMu.Lock()
	defer a.matMu.Unlock()
	if err := os.MkdirAll(a.emotionsDir, 0o755); err != nil {
		a.log.Warn("创建素材目录失败", "err", err)
		http.Error(w, "保存失败", http.StatusInternalServerError) // 不向客户端回显内部路径
		return
	}
	if err := a.saveMaterialBytes(name, ext, data); err != nil {
		a.log.Warn("保存素材失败", "name", name, "err", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	if err := a.reloadClips(); err != nil {
		a.log.Warn("素材热重载失败", "err", err)
	}
	a.broadcastMaterials()
	writeJSON(w, map[string]any{"ok": true, "name": name})
}

// handleVideoUpload 处理视频素材上传：用 ffmpeg 抽帧 → 落盘为 emotions/<情绪>/ 帧序列（含 fps）。
// 抽帧（重活）在锁外完成；仅“替换旧素材 + 热重载”持锁，且只有抽帧成功后才动旧素材，故失败不丢数据。
func (a *app) handleVideoUpload(w http.ResponseWriter, r *http.Request, name, ext string, file io.Reader) {
	if err := os.MkdirAll(a.emotionsDir, 0o755); err != nil {
		a.log.Warn("创建素材目录失败", "err", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}

	// 1) 把上传写到临时输入文件（ffmpeg 需要真实文件输入）。
	in, err := os.CreateTemp("", "esvideo-*"+ext)
	if err != nil {
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	inPath := in.Name()
	defer os.Remove(inPath)
	if _, err := io.Copy(in, file); err != nil {
		in.Close()
		http.Error(w, "读取上传内容失败", http.StatusBadRequest)
		return
	}
	in.Close()

	// 2) 抽帧到 emotions/ 下的暂存目录（同卷，便于稍后原子改名），传 0 用 ffmpeg 抽帧的默认帧数上限。
	stage := filepath.Join(a.emotionsDir, ".staging-"+name)
	_ = os.RemoveAll(stage)
	defer os.RemoveAll(stage)

	ctx, cancel := context.WithTimeout(r.Context(), videoTimeout)
	defer cancel()
	n, err := display.ExtractVideoFrames(ctx, a.ffmpegPath(), inPath, stage, videoFPS, 0)
	if err != nil {
		if errors.Is(err, display.ErrFFmpegNotFound) {
			http.Error(w, "服务器未安装 ffmpeg，无法处理视频（图片/GIF 不受影响）", http.StatusBadRequest)
			return
		}
		a.log.Warn("视频抽帧失败", "name", name, "err", err)
		http.Error(w, "视频无法解析（请确认是有效视频文件）", http.StatusBadRequest)
		return
	}
	if n == 0 {
		http.Error(w, "视频未抽到任何帧", http.StatusBadRequest)
		return
	}
	// 记录帧率，供加载时按视频速率播放（目录加载器读取 clip.json）。
	_ = os.WriteFile(filepath.Join(stage, "clip.json"), []byte(fmt.Sprintf(`{"fps":%d}`, videoFPS)), 0o644)

	// 3) 锁内原子替换旧素材并热重载（抽帧已成功，删旧素材安全）。
	a.matMu.Lock()
	defer a.matMu.Unlock()
	a.removeMaterialFiles(name)
	if err := os.Rename(stage, filepath.Join(a.emotionsDir, name)); err != nil {
		a.log.Warn("替换素材失败", "name", name, "err", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	if err := a.reloadClips(); err != nil {
		a.log.Warn("素材热重载失败", "err", err)
	}
	a.broadcastMaterials()
	writeJSON(w, map[string]any{"ok": true, "name": name, "frames": n})
}

// ffmpegPath 返回 ffmpeg 可执行路径：复用摄像头配置的路径，缺省为 "ffmpeg"（取自 PATH）。
func (a *app) ffmpegPath() string {
	if a.cfg != nil && a.cfg.Camera.FFmpeg != "" {
		return a.cfg.Camera.FFmpeg
	}
	return "ffmpeg"
}

// saveMaterialBytes 把已校验的素材字节写到 emotions/ 下的对应位置（覆盖同名旧素材）。
// 仅在 ValidateUpload 通过后调用，故 removeMaterialFiles 删旧素材是安全的——新内容必有效。
func (a *app) saveMaterialBytes(name, ext string, data []byte) error {
	switch ext {
	case ".gif":
		a.removeMaterialFiles(name)
		return os.WriteFile(filepath.Join(a.emotionsDir, name+".gif"), data, 0o644)
	case ".png", ".jpg", ".jpeg":
		a.removeMaterialFiles(name)
		dir := filepath.Join(a.emotionsDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "0001"+ext), data, 0o644)
	default:
		return fmt.Errorf("不支持的文件类型 %q（支持 .gif / .png / .jpg）", ext)
	}
}

// handleMaterialDelete 处理 material_delete 命令：删素材文件、热重载、广播新列表。
func (a *app) handleMaterialDelete(name string) {
	clean, ok := sanitizeMaterialName(name)
	if !ok {
		return
	}
	a.matMu.Lock()
	a.removeMaterialFiles(clean)
	err := a.reloadClips()
	a.matMu.Unlock()
	if err != nil {
		a.log.Warn("素材热重载失败", "err", err)
	}
	a.broadcastMaterials()
}

// handleMaterialThumb 返回某情绪动画首帧的 PNG 缩略图（240×240），供界面预览。
func (a *app) handleMaterialThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET", http.StatusMethodNotAllowed)
		return
	}
	name, ok := sanitizeMaterialName(r.URL.Query().Get("name"))
	if !ok {
		http.Error(w, "情绪名非法", http.StatusBadRequest)
		return
	}
	frame := a.clips.FirstFrame(name)
	if frame == nil || len(frame) != robot.ImageBytesRGB888 {
		http.NotFound(w, r)
		return
	}
	img := image.NewRGBA(image.Rect(0, 0, robot.ScreenWidth, robot.ScreenHeight))
	for i := 0; i < robot.ScreenWidth*robot.ScreenHeight; i++ {
		img.Pix[i*4] = frame[i*3]
		img.Pix[i*4+1] = frame[i*3+1]
		img.Pix[i*4+2] = frame[i*3+2]
		img.Pix[i*4+3] = 255
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_ = png.Encode(w, img)
}

// reloadClips 重新扫描 emotions/ 并原子替换播放集（热重载，立即生效）。
func (a *app) reloadClips() error {
	clips, err := display.LoadClips(a.emotionsDir)
	if err != nil {
		return err
	}
	a.clips.Replace(clips)
	a.log.Info("表情素材已重载", "情绪数", len(clips))
	return nil
}

// removeMaterialFiles 删除某情绪的所有可能落盘形式（gif / 目录帧 / 图集），确保不残留旧素材。
// name 已净化，base 必落在 emotionsDir 之内，不存在路径穿越。
func (a *app) removeMaterialFiles(name string) {
	base := filepath.Join(a.emotionsDir, name)
	for _, ext := range []string{".gif", ".json", ".png", ".jpg", ".jpeg"} {
		_ = os.Remove(base + ext)
	}
	_ = os.RemoveAll(base) // 目录形式（一帧一文件）
}

// materialsEvent 构造当前素材列表事件。
func (a *app) materialsEvent() protocol.MaterialsEvent {
	infos := a.clips.Info()
	out := make([]protocol.MaterialInfo, 0, len(infos))
	for _, ci := range infos {
		out = append(out, protocol.MaterialInfo{
			Name: ci.Name, Frames: ci.Frames, FPS: ci.FPS, Kind: a.materialKind(ci.Name),
		})
	}
	return protocol.MaterialsEvent{Materials: out}
}

// broadcastMaterials 把最新素材列表广播给所有客户端。
func (a *app) broadcastMaterials() { a.srv.Broadcast(a.materialsEvent()) }

// materialKind 依据落盘文件判断素材来源类型（仅用于界面展示）。
func (a *app) materialKind(name string) string {
	base := filepath.Join(a.emotionsDir, name)
	if fi, err := os.Stat(base + ".gif"); err == nil && !fi.IsDir() {
		return "gif"
	}
	if fi, err := os.Stat(base + ".json"); err == nil && !fi.IsDir() {
		return "atlas"
	}
	return "frames"
}

// sanitizeMaterialName 把用户输入的情绪名归一化为安全的文件名片段：
// 允许 Unicode 字母/数字（含中文）与 `_`、`-`，转小写；拒绝空、超长（>24 字）
// 以及任何其它字符——尤其是 `.` `/` `\` `:` 空白与控制符，从而杜绝路径穿越（../、绝对路径、隐藏文件）。
// 中文等字符作为文件名在 Windows/Linux 均安全，且不具路径意义。
func sanitizeMaterialName(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || utf8.RuneCountInString(s) > 24 {
		return "", false
	}
	for _, r := range s {
		ok := r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)
		if !ok {
			return "", false
		}
	}
	return s, true
}

// writeJSON 以 JSON 形式回写一个响应体。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
