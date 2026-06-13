// Package audioout 把一段 mp3 字节播放到设备侧扬声器。
//
// 受“主程序无 cgo”约束，播放交给外部 mpg123 子进程（与音乐模块同一依赖）：
// 用 `mpg123 -q -` 从 stdin 读取 mp3 并播放，一段语音=一次性子进程，播完即退出。
// 适合 MiniMax 云端 TTS 产出的 mp3「直接发设备播」这条路径。
package audioout

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
)

// Player 用 mpg123 子进程播放 mp3 字节。并发安全；同一时刻只播一段（新播放会打断旧的）。
type Player struct {
	bin string
	log *slog.Logger

	mu  sync.Mutex
	cur *exec.Cmd // 当前正在播放的子进程；nil 表示空闲
}

// New 创建播放器。bin 为空则用 "mpg123"（需在 PATH）。
func New(bin string, log *slog.Logger) *Player {
	if bin == "" {
		bin = "mpg123"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Player{bin: bin, log: log}
}

// Play 同步播放一段 mp3（阻塞到播完或被 Stop 打断）。调用方通常放 goroutine 里跑。
// 会先打断上一段。mpg123 不存在时返回错误（便于上层回退到页面播放）。
func (p *Player) Play(ctx context.Context, mp3 []byte) error {
	if len(mp3) == 0 {
		return nil
	}
	p.Stop() // 打断上一段

	cmd := exec.CommandContext(ctx, p.bin, "-q", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("audioout: 创建 stdin 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("audioout: 启动 mpg123 失败（确认已安装并在 PATH）: %w", err)
	}

	p.mu.Lock()
	p.cur = cmd
	p.mu.Unlock()

	// 喂数据后关闭 stdin，mpg123 读到 EOF 即播完退出。
	go func() {
		_, _ = stdin.Write(mp3)
		_ = stdin.Close()
	}()

	err = cmd.Wait()

	p.mu.Lock()
	if p.cur == cmd {
		p.cur = nil
	}
	p.mu.Unlock()

	// 被 Stop 杀掉时 Wait 会返回错误，这属正常打断，不上抛。
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Stop 打断当前播放（若有）。
func (p *Player) Stop() {
	p.mu.Lock()
	cmd := p.cur
	p.cur = nil
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
