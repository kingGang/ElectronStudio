package speech

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Sidecar 通过一条 WebSocket 连接对接外部语音 sidecar（如 sherpa-onnx + Piper）。
// 协议（JSON 文本帧，详见 docs/SPEECH.md）：
//
//	sidecar → 本端：{"type":"wake","keyword":"你好小电"}
//	                {"type":"vad","speaking":true,"level":0.6}
//	                {"type":"asr","text":"打开台灯","final":true}
//	本端 → sidecar：{"type":"speak","text":"好的"}
//	                {"type":"abort"}
type Sidecar struct {
	url string
	log *slog.Logger

	events chan Event

	mu        sync.Mutex
	conn      *websocket.Conn // 当前连接；nil 表示未连接
	connected bool
	writeTO   time.Duration
	onState   func() // 连接状态变化回调（连上/断开），用于刷新界面状态
}

// OnStateChange 注册连接状态变化回调（连上或断开时触发）。
func (s *Sidecar) OnStateChange(fn func()) { s.onState = fn }

func (s *Sidecar) notify() {
	if s.onState != nil {
		s.onState()
	}
}

// sidecarMsg 是与 sidecar 之间交换的 JSON 消息（双向复用同一结构）。
type sidecarMsg struct {
	Type     string  `json:"type"`
	Keyword  string  `json:"keyword,omitempty"`
	Speaking bool    `json:"speaking,omitempty"`
	Level    float32 `json:"level,omitempty"`
	Text     string  `json:"text,omitempty"`
	Final    bool    `json:"final,omitempty"`
	Format   string  `json:"format,omitempty"` // type=play：音频容器格式（如 ogg）
	Data     string  `json:"data,omitempty"`   // type=play：base64 编码的音频字节
}

// NewSidecar 创建一个连接到 wsURL 的语音 sidecar 客户端（尚未连接，需调用 Start）。
func NewSidecar(wsURL string, log *slog.Logger) *Sidecar {
	if log == nil {
		log = slog.Default()
	}
	return &Sidecar{
		url:     wsURL,
		log:     log,
		events:  make(chan Event, 32),
		writeTO: 5 * time.Second,
	}
}

// Start 实现 Service：启动后台连接管理（自动重连），非阻塞、不因 sidecar 未就绪而失败。
// sidecar 后启动、断线、重启都会被自动接上，连接状态变化会回调刷新界面。
func (s *Sidecar) Start(ctx context.Context) error {
	go s.manage(ctx)
	return nil
}

// manage 持续维护与 sidecar 的连接：连不上则退避重试；连上后跑读取循环，断开后自动重连。
func (s *Sidecar) manage(ctx context.Context) {
	const minBackoff, maxBackoff = 1 * time.Second, 5 * time.Second
	backoff := minBackoff
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, s.url, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if sleepCtx(ctx, backoff) {
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = minBackoff
		conn.SetReadLimit(1 << 20)
		s.mu.Lock()
		s.conn = conn
		s.connected = true
		s.mu.Unlock()
		s.log.Info("已连接语音 sidecar", "url", s.url)
		s.notify()

		s.readLoop(ctx) // 阻塞直到连接断开

		s.mu.Lock()
		s.conn = nil
		s.connected = false
		s.mu.Unlock()
		s.notify()
		if ctx.Err() != nil {
			return
		}
		s.log.Info("语音 sidecar 断开，准备重连", "url", s.url)
		if sleepCtx(ctx, minBackoff) {
			return
		}
	}
}

// sleepCtx 睡 d，期间 ctx 取消则提前返回 true。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

// Events 实现 Service。
func (s *Sidecar) Events() <-chan Event { return s.events }

// readLoop 持续读取 sidecar 消息并映射为 Event。
func (s *Sidecar) readLoop(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
	}()

	for {
		conn := s.currentConn()
		if conn == nil {
			return
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("语音 sidecar 读取结束", "err", err)
			}
			return
		}
		var m sidecarMsg
		if err := json.Unmarshal(data, &m); err != nil {
			s.log.Warn("语音 sidecar 消息解析失败", "err", err)
			continue
		}
		if ev, ok := toEvent(m); ok {
			select {
			case s.events <- ev:
			case <-ctx.Done():
				return
			default:
				s.log.Warn("语音事件队列已满，丢弃", "kind", ev.Kind)
			}
		}
	}
}

// toEvent 把 sidecar 消息转换为 Event；无法识别的类型返回 ok=false。
func toEvent(m sidecarMsg) (Event, bool) {
	switch EventKind(m.Type) {
	case KindWake:
		return Event{Kind: KindWake, Keyword: m.Keyword}, true
	case KindVAD:
		return Event{Kind: KindVAD, Speaking: m.Speaking, Level: m.Level}, true
	case KindASR:
		return Event{Kind: KindASR, Text: m.Text, Final: m.Final}, true
	default:
		return Event{}, false
	}
}

// Speak 实现 Service：请求 sidecar 合成并播放文本。
func (s *Sidecar) Speak(ctx context.Context, text string) error {
	return s.send(ctx, sidecarMsg{Type: "speak", Text: text})
}

// PlayAudio 请求 sidecar 解码并播放一段音频（如小智自带 TTS 的 Ogg/Opus）。
// 设备侧无 cgo，故把解码+播放交给 sidecar（Python + sounddevice），避免依赖 ffmpeg。
// data 为音频容器字节，format 为容器格式（如 "ogg"）。
func (s *Sidecar) PlayAudio(ctx context.Context, format string, data []byte) error {
	return s.send(ctx, sidecarMsg{
		Type:   "play",
		Format: format,
		Data:   base64.StdEncoding.EncodeToString(data),
	})
}

// Stop 实现 Service：请求 sidecar 打断当前播放。
func (s *Sidecar) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTO)
	defer cancel()
	if err := s.send(ctx, sidecarMsg{Type: "abort"}); err != nil {
		s.log.Warn("发送打断失败", "err", err)
	}
}

// send 把一条消息写给 sidecar（带超时；写操作串行化）。
func (s *Sidecar) send(ctx context.Context, m sidecarMsg) error {
	conn := s.currentConn()
	if conn == nil {
		return fmt.Errorf("speech: sidecar 未连接")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, s.writeTO)
	defer cancel()
	// coder/websocket 不允许并发写，这里用锁串行化。
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.Write(wctx, websocket.MessageText, data)
}

// Status 实现 Service。
func (s *Sidecar) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	detail := "sidecar"
	if !s.connected {
		detail = "sidecar 未连接"
	}
	return Status{ASRRunning: s.connected, TTSRunning: s.connected, Detail: detail}
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

// currentConn 安全地取当前连接。
func (s *Sidecar) currentConn() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}
