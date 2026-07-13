package display

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// ffmpegFilter 拼出送屏前的滤镜链：先按 rotate 转正、再按 mirror 左右翻，最后缩放到屏幕尺寸。
// 顺序与 macOS 进程内采集(incam_darwin.m：先旋转后镜像)一致，两条路出图才一样。
//
// 之所以要旋转：精英版把摄像头模组横着装进头里，出图天生躺倒（初代是正装的，rotate=0 即可）。
// 此前 rotate/mirror 只在 macOS 的进程内采集里生效，ffmpeg 这条路（Windows/Linux/树莓派）
// 压根没接，配了也没反应。
func ffmpegFilter(rotate int, mirror bool, w, h int) string {
	var vf []string
	switch ((rotate % 360) + 360) % 360 {
	case 90: // 顺时针 90°
		vf = append(vf, "transpose=1")
	case 180:
		vf = append(vf, "transpose=1", "transpose=1")
	case 270: // 顺时针 270° = 逆时针 90°
		vf = append(vf, "transpose=2")
	}
	if mirror {
		vf = append(vf, "hflip")
	}
	vf = append(vf, fmt.Sprintf("scale=%d:%d", w, h))
	return strings.Join(vf, ",")
}

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
	// Rotate/Mirror：送屏前对画面做的旋转(0|90|180|270，顺时针)与左右镜像。官方上位机不做任何
	// 变换（初代摄像头正装），精英版把摄像头横着装进头里、出图躺倒，故按机器配置。
	Rotate int
	Mirror bool
	// ScreenMode：true=摄像头到屏模式——【不】起独立 readFrames goroutine，改由设备同步 owner
	// 调 ReadFrame 串行拉帧(来一帧发一帧，仿官方 EmoticonActionFrameService 单一串行发送)，
	// 消除"并发读管道帧"这条与 libusb 争抢、卡死设备屏的路径。false=普通模式(vision 抓帧用)。
	ScreenMode bool
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

	pipe io.ReadCloser // ScreenMode：采集子进程 stdout，由 ReadFrame 串行拉帧（非屏幕模式为 nil）
	rbuf []byte        // ReadFrame 复用缓冲（仅设备同步 owner 单线程访问）

	inproc  bool          // ScreenMode 且 darwin+cgo：进程内 AVFoundation 采集（无子进程/管道）
	frameCh chan []byte   // 进程内采集的帧通道（缓冲 1、丢旧补新）；ReadFrame 从此阻塞取帧
	done    chan struct{} // 进程内采集停止信号；Stop 关闭它解阻塞 ReadFrame（不关 frameCh，避免发送方 panic）
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

	// 屏幕模式 + 原生后端 + darwin&cgo：走进程内 AVFoundation 采集(等价官方 MediaCapture)，
	// 无 camcap 子进程、无管道——根除"读管道帧与 libusb 争抢卡死设备屏"。每帧回调 pushInprocFrame。
	if cfg.ScreenMode && cfg.Backend == "native" && incamSupported() {
		c.mu.Lock()
		c.inproc = true
		c.frameCh = make(chan []byte, 1)
		c.done = make(chan struct{})
		c.running = true
		c.mu.Unlock()
		incamSetOrientation(cfg.Rotate, cfg.Mirror) // 精英版摄像头模组是横装的，出图需转正
		if err := incamStart(c, cfg.Input, scrW); err != nil {
			c.mu.Lock()
			c.inproc = false
			c.frameCh = nil
			c.done = nil
			c.running = false
			c.mu.Unlock()
			return err
		}
		c.log.Info("摄像头进程内采集已启动(cgo AVFoundation)", "input", cfg.Input)
		return nil
	}

	var cmd *exec.Cmd
	if cfg.Backend == "native" && cfg.Camcap != "" {
		// 原生 macOS AVFoundation 采集(无 ffmpeg)：camcap <摄像头名子串> <边长>，输出 240×240 rgb24。
		cmd = exec.CommandContext(ctx, cfg.Camcap, cfg.Input, fmt.Sprintf("%d", scrW))
	} else {
		ffmpeg := cfg.FFmpeg
		if ffmpeg == "" {
			ffmpeg = "ffmpeg"
		}
		// -fflags nobuffer：别为了平滑而攒帧，采到就吐（真正的丢旧帧在 pumpPipe 里做）。
		//
		// 【别加 -flags low_delay】：它是解码器标志，对 dshow 原始采集无意义，而且真机上会让
		// dshow 直接报 "Could not set video options" → I/O error，摄像头根本起不来。
		args := []string{
			"-loglevel", "error",
			"-fflags", "nobuffer",
			"-f", cfg.InputFormat,
		}
		// avfoundation 等需要在 -i 前指定支持的分辨率/帧率，否则报 Input/output error。
		if cfg.Framerate != "" {
			args = append(args, "-framerate", cfg.Framerate)
		}
		if cfg.VideoSize != "" {
			args = append(args, "-video_size", cfg.VideoSize)
		}
		args = append(args,
			"-i", cfg.Input,
			"-vf", ffmpegFilter(cfg.Rotate, cfg.Mirror, scrW, scrH),
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
	if cfg.ScreenMode {
		// 屏幕模式：起一个【只读管道】的泵，把最新一帧放进 1 格 channel（丢旧补新），
		// ReadFrame 从 channel 取——见 pumpPipe 的注释，这是画面延迟的根治。
		c.pipe = stdout
		c.frameCh = make(chan []byte, 1)
		c.done = make(chan struct{})
	}
	c.mu.Unlock()

	if cfg.ScreenMode {
		go c.pumpPipe(stdout)
	} else {
		go c.readFrames(stdout)
	}
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
		c.log.Info("摄像头采集已结束")
	}()
	c.log.Info("摄像头采集已启动", "input", cfg.Input, "screen", cfg.ScreenMode)
	return nil
}

// ReadFrame 阻塞读取一整帧（仅 ScreenMode）。由设备同步 owner【单线程串行】调用：拉到一帧
// 就 Sync 一帧——来一帧发一帧，仿官方 EmoticonActionFrameService 的单一串行发送，彻底取代
// "独立 readFrames goroutine + 自由 ticker syncLoop 并发读管道"这条卡死设备屏的路径。
// 拉到的帧同时存为 latest 供 Snapshot（vision 抓帧）。流结束/已停返回 nil。
func (c *CameraSource) ReadFrame() []byte {
	c.mu.Lock()
	ch := c.frameCh
	done := c.done
	c.mu.Unlock()

	// 两种采集后端（macOS 进程内 AVFoundation / 各平台 ffmpeg 子进程）现在都通过同一个
	// 1 格「丢旧补新」channel 供帧，故取帧路径统一：阻塞等下一帧，Stop 关 done 即解阻塞返回 nil。
	if ch == nil {
		return nil
	}
	select {
	case frame := <-ch:
		return frame
	case <-done:
		return nil
	}
}

// pumpPipe 把 ffmpeg 的裸帧流泵进「丢旧补新」的 1 格 channel（ScreenMode 专用）。
//
// 【为什么必须有它——画面延迟的根治】：以前 ReadFrame 直接 io.ReadFull(管道)，而管道是 FIFO，
// 每次取到的是【最老】的一帧。ffmpeg 以摄像头固有帧率生产（这台 UVC 只支持 30fps，改不了），
// 而 USB 同步实测只能消费约 17fps——多出来的帧就在 ffmpeg 内部队列和管道里排队，积压稳定之后
// 画面恒定滞后一整个队列深度：人动了，屏幕要过好几百毫秒才跟上，而且【永远追不回来】。
//
// 泵把管道抽干、只留最新一帧：不管生产多快，同步 owner 拿到的永远是刚采到的那一帧，延迟被钉死
// 在一帧以内。这正是 macOS 进程内采集那条路早就在做的事（pushInprocFrame 的丢旧补新），
// ffmpeg 这条路一直漏了。
func (c *CameraSource) pumpPipe(r io.Reader) {
	buf := make([]byte, robot.ImageBytesRGB888)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return // 流结束/进程退出（Stop 关管道即从这里解阻塞）
		}
		cp := make([]byte, len(buf))
		copy(cp, buf)
		c.pushInprocFrame(cp) // 存 latest（供 vision 抓帧）+ 丢旧补新塞进 channel
	}
}

// pushInprocFrame 接收进程内采集的一帧(由 incam 回调，AVFoundation 串行队列线程)：存为 latest
// 供 Snapshot，并丢旧补新地塞进 frameCh 供 ReadFrame 取——保证设备同步 owner 总拿到最新帧。
func (c *CameraSource) pushInprocFrame(b []byte) {
	c.mu.Lock()
	if len(c.latest) != len(b) {
		c.latest = make([]byte, len(b))
	}
	copy(c.latest, b)
	c.seq++
	ch := c.frameCh
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- b:
	default: // 通道已满:清掉旧帧再放新帧(丢旧补新)
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- b:
		default:
		}
	}
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

// Stop 停止采集并释放子进程/进程内采集。关闭管道或通道会让阻塞中的 ReadFrame 立即解阻塞返回 nil。
func (c *CameraSource) Stop() {
	c.mu.Lock()
	cmd := c.cmd
	pipe := c.pipe
	inproc := c.inproc
	done := c.done
	c.cmd = nil
	c.pipe = nil
	c.inproc = false
	c.frameCh = nil
	c.done = nil
	c.running = false
	c.latest = nil
	c.mu.Unlock()
	// 两条采集路径现在都让 ReadFrame 阻塞在 frameCh 上，所以【都】要关 done 才能解阻塞。
	// 不关 frameCh：在途的发送方（incam 回调 / pumpPipe）还可能往里塞，关了会 panic。
	if done != nil {
		close(done)
	}
	if inproc {
		incamStop() // 停 AVFoundation 采集（之后不再有 pushInprocFrame 回调）
		return
	}
	if pipe != nil {
		_ = pipe.Close() // 让 pumpPipe 的 ReadFull 出错返回、goroutine 退出
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
