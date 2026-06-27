package music

// DevicePlayer 把音乐播放到【指定输出设备】(macOS：sidecars/audio/musicto 子进程，
// 靠 NSSound 定向 USB 声卡，不改系统默认输出)。受“主程序无 cgo”约束，原生音频走这个
// 独立 Swift 小工具——与 TTS 的 playto 同源。常驻子进程，从 stdin 读命令(类似 mpg123 -R)：
// 下载到临时 mp3 后发 LOAD，PAUSE/RESUME/STOP/VOLUME 直传。无 musicto 时上层回退 mpg123。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
)

// DevicePlayer 实现 Player：经 musicto 把音乐播到指定设备。
type DevicePlayer struct {
	bin    string // musicto 可执行文件路径
	device string // 输出设备名子串
	log    *slog.Logger

	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.Writer
	state State
	tmp   string // 当前曲目的临时 mp3，换曲/关闭时清理
}

// NewDevicePlayer 创建设备音乐播放器。bin 为 musicto 路径，device 为输出设备名子串。
func NewDevicePlayer(bin, device string, log *slog.Logger) *DevicePlayer {
	if log == nil {
		log = slog.Default()
	}
	return &DevicePlayer{bin: bin, device: device, log: log, state: StateStopped}
}

// ensure 启动 musicto 常驻子进程（若未运行）。需持锁调用。
func (p *DevicePlayer) ensure() error {
	if p.cmd != nil {
		return nil
	}
	cmd := exec.Command(p.bin, p.device)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("music: musicto stdin 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("music: 启动 musicto 失败: %w", err)
	}
	p.cmd = cmd
	p.stdin = stdin
	go func() { _ = cmd.Wait(); p.onExit() }()
	return nil
}

func (p *DevicePlayer) onExit() {
	p.mu.Lock()
	p.cmd = nil
	p.stdin = nil
	p.state = StateStopped
	p.mu.Unlock()
}

// writeCmd 向 musicto 发一条命令。需持锁调用。
func (p *DevicePlayer) writeCmd(s string) error {
	if p.stdin == nil {
		return fmt.Errorf("music: musicto 未运行")
	}
	_, err := io.WriteString(p.stdin, s+"\n")
	return err
}

// Play 实现 Player：下载流地址到临时文件后交给 musicto 播放。
// NSSound 播文件不便流式播 URL，这里先在 Go 侧下载(后台，不阻塞)，完成即 LOAD。
func (p *DevicePlayer) Play(ctx context.Context, url string) error {
	p.mu.Lock()
	if err := p.ensure(); err != nil {
		p.mu.Unlock()
		return err
	}
	p.state = StatePlaying
	p.mu.Unlock()

	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			p.log.Warn("音乐请求构造失败", "err", err)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			p.log.Warn("音乐下载失败", "err", err)
			return
		}
		defer resp.Body.Close()
		f, err := os.CreateTemp("", "ebmusic-*.mp3")
		if err != nil {
			p.log.Warn("音乐临时文件失败", "err", err)
			return
		}
		_, err = io.Copy(f, resp.Body)
		_ = f.Close()
		if err != nil {
			p.log.Warn("音乐写入失败", "err", err)
			_ = os.Remove(f.Name())
			return
		}
		p.mu.Lock()
		old := p.tmp
		p.tmp = f.Name()
		err = p.writeCmd("LOAD " + f.Name())
		p.mu.Unlock()
		if err != nil {
			p.log.Warn("musicto LOAD 失败", "err", err)
		}
		if old != "" {
			_ = os.Remove(old) // 清理上一首
		}
	}()
	return nil
}

// Pause 实现 Player。
func (p *DevicePlayer) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != StatePlaying {
		return nil
	}
	err := p.writeCmd("PAUSE")
	p.state = StatePaused
	return err
}

// Resume 实现 Player。
func (p *DevicePlayer) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != StatePaused {
		return nil
	}
	err := p.writeCmd("RESUME")
	p.state = StatePlaying
	return err
}

// Stop 实现 Player。
func (p *DevicePlayer) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return nil
	}
	err := p.writeCmd("STOP")
	p.state = StateStopped
	return err
}

// SetVolume 实现 Player（0~100）。
func (p *DevicePlayer) SetVolume(percent int) error {
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensure(); err != nil {
		return err
	}
	return p.writeCmd(fmt.Sprintf("VOLUME %d", percent))
}

// State 实现 Player。
func (p *DevicePlayer) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Close 实现 Player：退出子进程并清理临时文件。
func (p *DevicePlayer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin != nil {
		_ = p.writeCmd("QUIT")
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	if p.tmp != "" {
		_ = os.Remove(p.tmp)
	}
	return nil
}
