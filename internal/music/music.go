// Package music 提供音乐搜索与播放。
//
// 受"主程序无 cgo"约束：搜索走纯 Go HTTP（酷我等），播放交给外部 mpg123 子进程
// （`mpg123 -R` 远程模式，可控暂停/停止/音量，与原 Verdure 项目一致）。
// 无 mpg123 时用 Mock，便于无依赖跑通逻辑与测试。
package music

import "context"

// Track 是一首歌。
type Track struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Artist   string `json:"artist"`
	URL      string `json:"url,omitempty"`      // 可播放的流地址
	Duration int    `json:"duration,omitempty"` // 秒
}

// Searcher 抽象音乐搜索源。
type Searcher interface {
	// Search 按关键词搜索，返回候选曲目（URL 可能为空，需 ResolveURL 解析）。
	Search(ctx context.Context, query string) ([]Track, error)
	// ResolveURL 取一首歌的可播放流地址。
	ResolveURL(ctx context.Context, t Track) (string, error)
}

// State 是播放状态。
type State string

const (
	StateStopped State = "stopped"
	StatePlaying State = "playing"
	StatePaused  State = "paused"
)

// Player 抽象音频播放器。
type Player interface {
	Play(ctx context.Context, url string) error
	Pause() error
	Resume() error
	Stop() error
	SetVolume(percent int) error
	State() State
	Close() error
}
