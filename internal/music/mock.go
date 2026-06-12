package music

import "context"

// MockPlayer 是无音频的播放器（无 mpg123 时使用）：只记录状态，便于跑通逻辑与测试。
type MockPlayer struct {
	state  State
	lastURL string
	volume  int
}

// NewMockPlayer 创建一个 Mock 播放器。
func NewMockPlayer() *MockPlayer { return &MockPlayer{state: StateStopped, volume: 50} }

func (p *MockPlayer) Play(_ context.Context, url string) error {
	p.lastURL = url
	p.state = StatePlaying
	return nil
}
func (p *MockPlayer) Pause() error        { p.state = StatePaused; return nil }
func (p *MockPlayer) Resume() error       { p.state = StatePlaying; return nil }
func (p *MockPlayer) Stop() error         { p.state = StateStopped; return nil }
func (p *MockPlayer) SetVolume(v int) error { p.volume = v; return nil }
func (p *MockPlayer) State() State        { return p.state }
func (p *MockPlayer) Close() error        { return nil }

// MockSearcher 返回固定结果（无网络时演示/测试用）。
type MockSearcher struct{}

func (MockSearcher) Search(_ context.Context, query string) ([]Track, error) {
	return []Track{{ID: "1", Name: query, Artist: "演示歌手", URL: "http://example.com/song.mp3", Duration: 200}}, nil
}
func (MockSearcher) ResolveURL(_ context.Context, t Track) (string, error) {
	if t.URL != "" {
		return t.URL, nil
	}
	return "http://example.com/song.mp3", nil
}
