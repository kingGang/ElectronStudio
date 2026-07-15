package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Client 是一次实时语音会话。生命周期：Connect → (Events 消费 + PushAudio 推流) → Close。
// 由上层在【唤醒后】建立、聊完/超时后 Close（省流量与费用）。并发：PushAudio / 触发 / 打断
// 可从多个 goroutine 调，用一把写锁串行化对连接的写。
type Client struct {
	backend Backend
	log     *slog.Logger

	mu   sync.Mutex // 保护对 conn 的写（coder/websocket 不允许并发写）
	conn *websocket.Conn

	events chan Event
	closed chan struct{}
	once   sync.Once

	dbgAudio   atomic.Uint64 // 收到的下行音频块计数（联调可观测）
	dbgPushed  atomic.Uint64 // 推上去的上行音频块计数
}

// New 创建一个未连接的 Client。
func New(backend Backend, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		backend: backend,
		log:     log,
		events:  make(chan Event, 64),
		closed:  make(chan struct{}),
	}
}

// Events 返回对外事件流（转写 / 音频 / 函数调用 / 打断 / 结束 / 错误）。Close 后关闭。
func (c *Client) Events() <-chan Event { return c.events }

// Connect 建立 WebSocket、发出 session.update（人设 + 工具），并启动读循环。
func (c *Client) Connect(ctx context.Context, persona string, tools []ToolDef) error {
	h := http.Header{}
	h.Set("Authorization", c.backend.AuthHeader())

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, c.backend.DialURL(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return fmt.Errorf("realtime: 连接失败: %w", err)
	}
	conn.SetReadLimit(16 << 20) // 音频分片可能较大
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// 声明工具（按后端结构序列化）+ 发 session.update。
	rawTools, err := c.backend.EncodeTools(tools)
	if err != nil {
		_ = c.Close()
		return fmt.Errorf("realtime: 序列化工具失败: %w", err)
	}
	sess := c.backend.BuildSession(persona, rawTools)
	if err := c.write(ctx, sessionUpdateMsg{Type: EvSessionUpdate, Session: sess}); err != nil {
		_ = c.Close()
		return fmt.Errorf("realtime: 发送 session.update 失败: %w", err)
	}

	go c.readLoop()
	c.log.Info("realtime 会话已建立", "url", c.backend.DialURL())
	return nil
}

// PushAudio 推一段上行音频（原始 PCM，本函数负责 base64）。服务端 VAD 会自动断句并生成回复。
func (c *Client) PushAudio(ctx context.Context, pcm []byte) error {
	if n := c.dbgPushed.Add(1); n%50 == 1 {
		c.log.Info("realtime 已上行音频", "累计块", n, "本块字节", len(pcm))
	}
	b64 := base64.StdEncoding.EncodeToString(pcm)
	return c.write(ctx, appendAudioMsg{Type: EvInputAudioAppend, Audio: b64})
}

// SendText 以文本发起一轮（供打字输入 / 测试）：插入 user 消息并触发回复。
func (c *Client) SendText(ctx context.Context, text string) error {
	item := conversationItem{
		ItemType: "message", Role: "user",
		Content: []contentPart{{PartType: "input_text", Text: text}},
	}
	if err := c.write(ctx, createItemMsg{Type: EvConversationCreate, Item: item}); err != nil {
		return err
	}
	return c.write(ctx, simpleMsg{Type: EvResponseCreate})
}

// SendFunctionResult 回传一次工具执行结果并触发模型续答。callID 来自 KindFunctionCall 事件。
func (c *Client) SendFunctionResult(ctx context.Context, callID, output string) error {
	item := c.backend.FunctionOutputItem(callID, output)
	if err := c.write(ctx, createItemMsg{Type: EvConversationCreate, Item: item}); err != nil {
		return err
	}
	return c.write(ctx, simpleMsg{Type: EvResponseCreate})
}

// Cancel 打断进行中的回复（用户插话时调）。服务端 VAD 通常也会自动打断，这里是手动兜底。
func (c *Client) Cancel(ctx context.Context) error {
	return c.write(ctx, simpleMsg{Type: EvResponseCancel})
}

// Close 关闭会话，幂等。
func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.mu.Lock()
		conn := c.conn
		c.conn = nil
		c.mu.Unlock()
		if conn != nil {
			conn.Close(websocket.StatusNormalClosure, "")
		}
		close(c.events)
	})
	return nil
}

// write 串行化对连接的 JSON 写。
func (c *Client) write(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("realtime: 连接已关闭")
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

// readLoop 持续读事件、分派为对外 Event，直到连接关闭。
func (c *Client) readLoop() {
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			return
		}
		_, data, err := conn.Read(context.Background())
		if err != nil {
			select {
			case <-c.closed: // 主动关闭，静默退出
			default:
				c.log.Debug("realtime 读取结束", "err", err)
			}
			return
		}
		c.dispatch(data)
	}
}

// dispatch 把一条服务端消息翻译为对外 Event。未知事件忽略。
func (c *Client) dispatch(data []byte) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	// 联调期可观测：非高频事件逐条打 Info；音频增量只累计计数（避免刷屏）。见 dbgCount。
	if env.Type != EvAudioDelta {
		c.log.Info("realtime 收到事件", "type", env.Type)
	}
	switch env.Type {
	case EvAudioDelta:
		var m audioDelta
		if json.Unmarshal(data, &m) == nil && m.Delta != "" {
			if pcm, err := base64.StdEncoding.DecodeString(m.Delta); err == nil {
				if n := c.dbgAudio.Add(1); n%20 == 1 {
					c.log.Info("realtime 收到音频增量", "累计块", n)
				}
				c.emit(Event{Kind: KindAudio, Audio: pcm})
			}
		}
	case EvAudioTranscriptDone:
		var m audioTranscriptDone
		if json.Unmarshal(data, &m) == nil {
			c.emit(Event{Kind: KindAssistantText, Text: m.Transcript})
		}
	case EvInputTranscriptionDone:
		var m inputTranscriptionDone
		if json.Unmarshal(data, &m) == nil {
			c.emit(Event{Kind: KindUserTranscript, Text: m.Transcript})
		}
	case EvFunctionCallArgsDone:
		var m functionCallDone
		if json.Unmarshal(data, &m) == nil {
			c.emit(Event{Kind: KindFunctionCall, CallID: m.CallID, FuncName: m.Name, FuncArgs: m.Arguments})
		}
	case EvSpeechStarted:
		c.emit(Event{Kind: KindSpeechStarted})
	case EvResponseCreated:
		c.emit(Event{Kind: KindResponseStarted})
	case EvResponseDone:
		c.emit(Event{Kind: KindResponseDone})
	case EvError:
		var m errorEvent
		if json.Unmarshal(data, &m) == nil {
			c.log.Warn("realtime 服务端错误", "code", m.Error.Code, "msg", m.Error.Message)
			c.emit(Event{Kind: KindError, Text: m.Error.Message})
		}
	default:
		// session.created/updated、speech_stopped、audio.done 等：无需对外，忽略。
	}
}

// emit 非阻塞地投递一个对外事件（消费者慢则丢弃音频/转写，不阻塞读循环）。
func (c *Client) emit(ev Event) {
	select {
	case c.events <- ev:
	case <-c.closed:
	default:
		// 队列满：丢弃。音频/转写是实时流，积压无意义（宁可丢帧不要卡读循环）。
		c.log.Debug("realtime 事件队列满，丢弃", "kind", ev.Kind)
	}
}
