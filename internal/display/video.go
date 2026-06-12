package display

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrFFmpegNotFound 表示系统未安装 ffmpeg（或不在 PATH / 配置路径），无法处理视频。
var ErrFFmpegNotFound = errors.New("display: 未找到 ffmpeg")

// ExtractVideoFrames 用 ffmpeg 把视频抽帧为 outDir/%04d.png（缩放并居中裁剪到 240×240）。
//
// 受「主程序无 cgo」约束，视频解码交给外部 ffmpeg 子进程（与摄像头采集同一依赖）。
// 以固定 fps 重采样并用 -frames:v 限制帧数，从而约束输出规模（防超长视频耗尽磁盘/内存）；
// 通过 ctx 施加超时，避免恶意/异常输入让 ffmpeg 卡死。返回实际抽出的帧数。
func ExtractVideoFrames(ctx context.Context, ffmpeg, input, outDir string, fps, maxFrames int) (int, error) {
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if fps <= 0 {
		fps = clipFPS
	}
	if maxFrames <= 0 {
		maxFrames = maxGIFFrames
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, err
	}
	// fps 重采样 + 等比缩放到填满 240×240 后居中裁剪（避免拉伸变形），lanczos 高质量缩放。
	vf := fmt.Sprintf("fps=%d,scale=%d:%d:force_original_aspect_ratio=increase:flags=lanczos,crop=%d:%d",
		fps, scrW, scrH, scrW, scrH)
	args := []string{
		"-v", "error", "-y",
		"-i", input,
		"-vf", vf,
		"-frames:v", strconv.Itoa(maxFrames),
		filepath.Join(outDir, "%04d.png"),
	}
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return 0, ErrFFmpegNotFound
		}
		return 0, fmt.Errorf("display: ffmpeg 抽帧失败: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	// 统计抽出的帧数。
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".png") {
			n++
		}
	}
	return n, nil
}
