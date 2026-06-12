package music

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
)

// Mpg123Player 用外部 mpg123 的远程模式（`mpg123 -R`）播放并控制音频。
// 远程模式从 stdin 读命令：LOAD <url> / PAUSE(切换) / STOP / VOLUME <pct> / QUIT。
type Mpg123Player struct {
	bin string
	log *slog.Logger

	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.Writer
	state State
}

// NewMpg123Player 创建播放器。bin 为空则用 "mpg123"。
func NewMpg123Player(bin string, log *slog.Logger) *Mpg123Player {
	if bin == "" {
		bin = "mpg123"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Mpg123Player{bin: bin, log: log, state: StateStopped}
}

// ensure 启动 mpg123 -R 子进程（若未运行）。需持锁调用。
func (p *Mpg123Player) ensure() error {
	if p.cmd != nil {
		return nil
	}
	cmd := exec.Command(p.bin, "-R", "--remote-err")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("music: mpg123 stdin 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("music: 启动 mpg123 失败（确认已安装）: %w", err)
	}
	p.cmd = cmd
	p.stdin = stdin
	go func() { _ = cmd.Wait(); p.onExit() }()
	return nil
}

func (p *Mpg123Player) onExit() {
	p.mu.Lock()
	p.cmd = nil
	p.stdin = nil
	p.state = StateStopped
	p.mu.Unlock()
}

// writeCmd 向 mpg123 发送一条远程命令。需持锁调用。
func (p *Mpg123Player) writeCmd(s string) error {
	if p.stdin == nil {
		return fmt.Errorf("music: mpg123 未运行")
	}
	_, err := io.WriteString(p.stdin, s+"\n")
	return err
}

// Play 实现 Player：加载并播放一个流地址。
func (p *Mpg123Player) Play(_ context.Context, url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensure(); err != nil {
		return err
	}
	if err := p.writeCmd("LOAD " + url); err != nil {
		return err
	}
	p.state = StatePlaying
	return nil
}

// Pause 实现 Player。
func (p *Mpg123Player) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != StatePlaying {
		return nil
	}
	if err := p.writeCmd("PAUSE"); err != nil {
		return err
	}
	p.state = StatePaused
	return nil
}

// Resume 实现 Player。
func (p *Mpg123Player) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != StatePaused {
		return nil
	}
	if err := p.writeCmd("PAUSE"); err != nil { // PAUSE 切换，再发一次即恢复
		return err
	}
	p.state = StatePlaying
	return nil
}

// Stop 实现 Player。
func (p *Mpg123Player) Stop() error {
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
func (p *Mpg123Player) SetVolume(percent int) error {
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
func (p *Mpg123Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Close 实现 Player：退出子进程。
func (p *Mpg123Player) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin != nil {
		_ = p.writeCmd("QUIT")
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return nil
}
