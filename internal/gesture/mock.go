package gesture

import (
	"context"
	"log/slog"
	"sync"
)

// Mock 是无真实识别的 Service 实现（无 sidecar 时使用）。可用 Inject 手动注入手势用于测试/调试。
type Mock struct {
	log    *slog.Logger
	events chan Event

	mu     sync.Mutex
	closed bool
}

// NewMock 创建一个 Mock 手势服务。
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

// Inject 注入一条手势事件（测试/调试用）。
func (m *Mock) Inject(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	select {
	case m.events <- e:
	default:
		m.log.Warn("Mock 手势队列已满，丢弃", "name", e.Name)
	}
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
