package speech

import (
	"context"
	"log/slog"
	"sync"
)

// Mock 是无真实音频的 Service 实现，用于无 sidecar 环境下跑通链路与测试。
//
// 它不主动产生语音事件（没有麦克风），但提供 Inject 以便测试或调试时手动注入；
// Speak 仅记录日志并不实际发声。
type Mock struct {
	log    *slog.Logger
	events chan Event

	mu     sync.Mutex
	closed bool
}

// NewMock 创建一个 Mock 语音服务。logger 为 nil 时用 slog 默认 logger。
func NewMock(log *slog.Logger) *Mock {
	if log == nil {
		log = slog.Default()
	}
	return &Mock{log: log, events: make(chan Event, 16)}
}

// Start 实现 Service：无操作。
func (m *Mock) Start(context.Context) error { return nil }

// Events 实现 Service。
func (m *Mock) Events() <-chan Event { return m.events }

// Inject 向事件流注入一条事件（测试 / 调试用）。已关闭后调用将被忽略。
func (m *Mock) Inject(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	select {
	case m.events <- e:
	default:
		m.log.Warn("Mock 语音事件队列已满，丢弃", "kind", e.Kind)
	}
}

// Speak 实现 Service：仅记录日志。
func (m *Mock) Speak(_ context.Context, text string) error {
	m.log.Info("TTS(mock)", "text", text)
	return nil
}

// Stop 实现 Service：无操作。
func (m *Mock) Stop() {}

// Status 实现 Service。
func (m *Mock) Status() Status {
	return Status{ASRRunning: true, TTSRunning: true, Detail: "mock"}
}

// Close 实现 Service。
func (m *Mock) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.events)
	}
	return nil
}
