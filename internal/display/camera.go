package display

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// CameraConfig 配置摄像头采集（由 ffmpeg 抓取 UVC 摄像头并缩放为 240×240 RGB24）。
type CameraConfig struct {
	FFmpeg      string // ffmpeg 路径，空则取 "ffmpeg"
	InputFormat string // v4l2 / dshow / avfoundation
	Input       string // 设备规格，如 /dev/video0 或 video=Integrated Camera
	// VideoSize/Framerate：采集分辨率与帧率（如 "640x480"/"30"）。avfoundation(macOS) 必须给一组
	// 摄像头支持的值，否则 ffmpeg 报 Input/output error；留空则不传（Linux v4l2 通常可省）。
	VideoSize string
	Framerate string
	// Backend："" / "ffmpeg"=用 ffmpeg；"native"=用原生 macOS 采集小工具 camcap(无 ffmpeg 依赖)，
	// 此时 Input 当作摄像头名子串、Camcap 为其可执行路径。
	Backend string
	Camcap  string
}

// CameraSource 把摄像头画面作为屏幕画面源。
//
// 受"主程序无 cgo"约束，采集交给外部 ffmpeg：以子进程抓取摄像头、缩放到 240×240、
// 输出 rgb24 裸视频到 stdout；Go 侧只按帧长(172800B)读取，纯 Go、可交叉编译。
// ElectronBot 的摄像头是板载 USB Hub 上的标准 UVC 设备，主机按普通 webcam 读取即可。
type CameraSource struct {
	log *slog.Logger

	mu      sync.Mutex
	latest  []byte
	seq     uint64 // 已采集帧计数
	lastSeq uint64 // 上次 Frame() 返回的帧计数
	cmd     *exec.Cmd
	running bool
}

// NewCameraSource 创建一个摄像头源（尚未启动）。
func NewCameraSource(log *slog.Logger) *CameraSource {
	if log == nil {
		log = slog.Default()
	}
	return &CameraSource{log: log}
}

// Start 启动 ffmpeg 采集子进程并开始读帧。
func (c *CameraSource) Start(ctx context.Context, cfg CameraConfig) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	var cmd *exec.Cmd
	if cfg.Backend == "native" && cfg.Camcap != "" {
		// 原生 macOS AVFoundation 采集(无 ffmpeg)：camcap <摄像头名子串> <边长>，输出 240×240 rgb24。
		cmd = exec.CommandContext(ctx, cfg.Camcap, cfg.Input, fmt.Sprintf("%d", scrW))
	} else {
		ffmpeg := cfg.FFmpeg
		if ffmpeg == "" {
			ffmpeg = "ffmpeg"
		}
		args := []string{"-loglevel", "error", "-f", cfg.InputFormat}
		// avfoundation 等需要在 -i 前指定支持的分辨率/帧率，否则报 Input/output error。
		if cfg.Framerate != "" {
			args = append(args, "-framerate", cfg.Framerate)
		}
		if cfg.VideoSize != "" {
			args = append(args, "-video_size", cfg.VideoSize)
		}
		args = append(args,
			"-i", cfg.Input,
			"-vf", fmt.Sprintf("scale=%d:%d", scrW, scrH),
			"-pix_fmt", "rgb24",
			"-f", "rawvideo", "-",
		)
		cmd = exec.CommandContext(ctx, ffmpeg, args...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("display: 摄像头采集管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("display: 启动摄像头采集失败: %w", err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.running = true
	c.mu.Unlock()

	go c.readFrames(stdout)
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
		c.log.Info("摄像头采集已结束")
	}()
	c.log.Info("摄像头采集已启动", "input", cfg.Input)
	return nil
}

// readFrames 从 r 按固定帧长连续读取 rgb24 帧，保存最新一帧。导出逻辑便于测试。
func (c *CameraSource) readFrames(r io.Reader) {
	buf := make([]byte, robot.ImageBytesRGB888)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return // 流结束或进程退出
		}
		cp := make([]byte, len(buf))
		copy(cp, buf)
		c.mu.Lock()
		c.latest = cp
		c.seq++
		c.mu.Unlock()
	}
}

// Frame 实现 Source：返回最新摄像头帧；若自上次以来无新帧则返回 nil。
func (c *CameraSource) Frame() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latest == nil || c.seq == c.lastSeq {
		return nil
	}
	c.lastSeq = c.seq
	out := make([]byte, len(c.latest))
	copy(out, c.latest)
	return out
}

// Snapshot 返回最近一帧的副本（不论是否已被 Frame 取过）；无帧返回 nil。
// 用于"看一眼"（抓帧送视觉模型），与显示用的 Frame 解耦。
func (c *CameraSource) Snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latest == nil {
		return nil
	}
	out := make([]byte, len(c.latest))
	copy(out, c.latest)
	return out
}

// Running 报告采集是否在运行。
func (c *CameraSource) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// Stop 停止采集并释放子进程。
func (c *CameraSource) Stop() {
	c.mu.Lock()
	cmd := c.cmd
	c.cmd = nil
	c.running = false
	c.latest = nil
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
