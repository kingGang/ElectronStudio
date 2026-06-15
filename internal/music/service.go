package music

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Service 把搜索与播放组合起来，并在状态变化时回调上层（用于广播给 UI）。
type Service struct {
	searcher Searcher
	player   Player
	log      *slog.Logger
	onState  func(Track, State)

	mu       sync.Mutex
	current  Track
	playlist []Track // 当前搜索结果作为播放列表，用于连续播放
	idx      int     // 当前曲目在 playlist 中的下标
	state    State   // 后端维护的播放状态(playing/paused/stopped)，供刷新重连恢复
	pos      float64 // 页面上报的当前进度(秒)，供重连 seek
}

// Playback 是供前端恢复用的播放快照。
type Playback struct {
	Track    Track
	State    State
	Position float64
}

// Snapshot 返回当前播放快照（用于新连接进来时恢复界面状态）。
func (s *Service) Snapshot() Playback {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	if st == "" {
		st = StateStopped
	}
	return Playback{Track: s.current, State: st, Position: s.pos}
}

// SetProgress 接收页面上报的播放进度/状态，落到后端（刷新后据此恢复）。
func (s *Service) SetProgress(pos float64, playing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateStopped {
		return // 已停止则忽略迟到的上报
	}
	s.pos = pos
	if playing {
		s.state = StatePlaying
	} else {
		s.state = StatePaused
	}
}

// NewService 创建音乐服务。onState 可为 nil。
func NewService(searcher Searcher, player Player, log *slog.Logger, onState func(Track, State)) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{searcher: searcher, player: player, log: log, onState: onState}
}

// SearchAndPlay 搜索关键词，把结果存为播放列表并从第一首开始播放（用于连续播放）。
func (s *Service) SearchAndPlay(ctx context.Context, query string) (Track, error) {
	tracks, err := s.searcher.Search(ctx, query)
	if err != nil {
		return Track{}, err
	}
	if len(tracks) == 0 {
		return Track{}, fmt.Errorf("music: 未搜到 %q", query)
	}
	s.mu.Lock()
	prevID := s.current.ID
	s.playlist = tracks
	// 确定性保证：若重搜结果的第一首正是当前在放的（典型"换一首"被理解成重搜的情况），
	// 自动从第二首起播，绝不"换成同一首"。
	start := 0
	if len(tracks) > 1 && tracks[0].ID == prevID && prevID != "" {
		start = 1
	}
	s.idx = start - 1 // 让下面的 advance(+1) 落到 start
	s.mu.Unlock()
	return s.advance(ctx, +1)
}

// Next 播放下一首（连续播放/手动切歌）。列表到底则循环回开头。
func (s *Service) Next(ctx context.Context) (Track, error) { return s.advance(ctx, +1) }

// Prev 播放上一首。
func (s *Service) Prev(ctx context.Context) (Track, error) { return s.advance(ctx, -1) }

// advance 从当前下标按 step 方向移动并播放，遇到无法取流的曲目自动跳过（保证连续播放
// 不会因个别无版权曲卡死）；整张列表都放不了才报错。step 为 +1/-1。
func (s *Service) advance(ctx context.Context, step int) (Track, error) {
	s.mu.Lock()
	n := len(s.playlist)
	start := s.idx
	s.mu.Unlock()
	if n == 0 {
		return Track{}, fmt.Errorf("music: 播放列表为空")
	}
	for k := 1; k <= n; k++ {
		i := ((start+step*k)%n + n) % n // 环形，支持负方向
		s.mu.Lock()
		t := s.playlist[i]
		s.mu.Unlock()
		url, err := s.searcher.ResolveURL(ctx, t)
		if err != nil {
			s.log.Warn("跳过无法播放的曲目", "name", t.Name, "artist", t.Artist, "err", err)
			continue
		}
		t.URL = url
		s.mu.Lock()
		s.idx = i
		s.current = t
		s.playlist[i] = t
		s.state = StatePlaying
		s.pos = 0 // 新曲从头
		s.mu.Unlock()
		// 设备侧播放器（mpg123）尽力启动；失败不致命——页面侧仍可凭事件里的 URL 在浏览器播放。
		if err := s.player.Play(ctx, url); err != nil {
			s.log.Warn("设备播放器启动失败，改由页面播放（如需设备出声请安装 mpg123）", "err", err)
		}
		s.emit(t, StatePlaying)
		s.log.Info("播放音乐", "name", t.Name, "artist", t.Artist, "idx", i, "of", n)
		return t, nil
	}
	return Track{}, fmt.Errorf("music: 播放列表里没有可播放的曲目")
}

// Pause / Resume / Stop / SetVolume 控制播放。
// 广播的是"意图状态"而非设备播放器状态——页面播放时浏览器据此暂停/继续 <audio>
// （设备无 mpg123 时其状态不可靠）。
func (s *Service) Pause() {
	_ = s.player.Pause()
	s.mu.Lock()
	s.state = StatePaused
	s.mu.Unlock()
	s.emit(s.Current(), StatePaused)
}
func (s *Service) Resume() {
	_ = s.player.Resume()
	s.mu.Lock()
	s.state = StatePlaying
	s.mu.Unlock()
	s.emit(s.Current(), StatePlaying)
}
func (s *Service) Stop() {
	_ = s.player.Stop()
	s.mu.Lock()
	s.state = StateStopped
	s.pos = 0
	s.mu.Unlock()
	s.emit(s.Current(), StateStopped)
}
func (s *Service) SetVolume(v int) { _ = s.player.SetVolume(v) }

// Current 返回当前曲目。
func (s *Service) Current() Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// State 返回当前播放状态。
func (s *Service) State() State { return s.player.State() }

// HasPlaylist 是否已有播放列表（用于判断"换一首"能否直接切歌）。
func (s *Service) HasPlaylist() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.playlist) > 0
}

// SetSearcher 热替换音源（如扫码登录成功后用带新 cookie 的搜索器替换旧的）。
func (s *Service) SetSearcher(se Searcher) {
	s.mu.Lock()
	s.searcher = se
	s.mu.Unlock()
}

// Close 释放播放器。
func (s *Service) Close() error { return s.player.Close() }

func (s *Service) emit(t Track, st State) {
	if s.onState != nil {
		s.onState(t, st)
	}
}
