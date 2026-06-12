package gesture

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/coder/websocket"
)

// Sidecar 通过 WebSocket 对接外部手势 sidecar（如 MediaPipe）。
// 协议（JSON 文本帧）：sidecar → 本端 {"type":"gesture","name":"wave","confidence":0.9}
type Sidecar struct {
	url string
	log *slog.Logger

	events chan Event

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool
}

type sidecarMsg struct {
	Type       string  `json:"type"`
	Name       string  `json:"name"`
	Confidence float32 `json:"confidence"`
}

// NewSidecar 创建一个连接到 wsURL 的手势 sidecar 客户端。
func NewSidecar(wsURL string, log *slog.Logger) *Sidecar {
	if log == nil {
		log = slog.Default()
	}
	return &Sidecar{url: wsURL, log: log, events: make(chan Event, 32)}
}

// Start 实现 Service：连接并启动读取循环。
func (s *Sidecar) Start(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, s.url, nil)
	if err != nil {
		return fmt.Errorf("gesture: 连接 sidecar 失败: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	s.mu.Lock()
	s.conn = conn
	s.connected = true
	s.mu.Unlock()
	s.log.Info("已连接手势 sidecar", "url", s.url)
	go s.readLoop(ctx)
	return nil
}

// Events 实现 Service。
func (s *Sidecar) Events() <-chan Event { return s.events }

func (s *Sidecar) readLoop(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()
		if conn == nil {
			return
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("手势 sidecar 读取结束", "err", err)
			}
			return
		}
		var m sidecarMsg
		if err := json.Unmarshal(data, &m); err != nil || m.Type != "gesture" || m.Name == "" {
			continue
		}
		select {
		case s.events <- Event{Name: m.Name, Confidence: m.Confidence}:
		case <-ctx.Done():
			return
		default:
			s.log.Warn("手势事件队列已满，丢弃", "name", m.Name)
		}
	}
}

// Close 实现 Service。
func (s *Sidecar) Close() error {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.connected = false
	s.mu.Unlock()
	if conn != nil {
		return conn.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}
