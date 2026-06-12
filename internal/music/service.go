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

	mu      sync.Mutex
	current Track
}

// NewService 创建音乐服务。onState 可为 nil。
func NewService(searcher Searcher, player Player, log *slog.Logger, onState func(Track, State)) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{searcher: searcher, player: player, log: log, onState: onState}
}

// SearchAndPlay 搜索关键词并播放第一条结果，返回所播曲目。
func (s *Service) SearchAndPlay(ctx context.Context, query string) (Track, error) {
	tracks, err := s.searcher.Search(ctx, query)
	if err != nil {
		return Track{}, err
	}
	if len(tracks) == 0 {
		return Track{}, fmt.Errorf("music: 未搜到 %q", query)
	}
	t := tracks[0]
	url, err := s.searcher.ResolveURL(ctx, t)
	if err != nil {
		return Track{}, err
	}
	t.URL = url
	if err := s.player.Play(ctx, url); err != nil {
		return Track{}, err
	}
	s.mu.Lock()
	s.current = t
	s.mu.Unlock()
	s.emit(t, StatePlaying)
	s.log.Info("播放音乐", "name", t.Name, "artist", t.Artist)
	return t, nil
}

// Pause / Resume / Stop / SetVolume 控制播放。
func (s *Service) Pause() {
	_ = s.player.Pause()
	s.emit(s.Current(), s.player.State())
}
func (s *Service) Resume() {
	_ = s.player.Resume()
	s.emit(s.Current(), s.player.State())
}
func (s *Service) Stop() {
	_ = s.player.Stop()
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

// Close 释放播放器。
func (s *Service) Close() error { return s.player.Close() }

func (s *Service) emit(t Track, st State) {
	if s.onState != nil {
		s.onState(t, st)
	}
}
