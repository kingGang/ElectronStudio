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

	// mu 只保护下面这几个【瞬时状态】字段，临界区必须短——Status() 也要拿它，而 Status() 在
	// statusSnapshot() 里、每个网页连上时都会走。绝不可在持有它时做网络 I/O（见 send 的注释）。
	mu        sync.Mutex
	conn      *websocket.Conn // 当前连接；nil 表示未连接
	connected bool
	voice     Voice // sidecar 上报的 TTS 音色能力与当前取值（"voice" 消息）
	writeTO   time.Duration
	onState   func() // 连接状态变化回调（连上/断开），用于刷新界面状态

	// writeMu 单独串行化 conn.Write（coder/websocket 不允许并发写）。与 mu 分开，是为了让一次
	// 慢写不会连累 Status() —— 那会让整个界面在 OnConnect 挂死。
	writeMu sync.Mutex
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
	Data     string  `json:"data,omitempty"`   // type=play/play_pcm/audio：base64 音频字节
	SampleRate int   `json:"sample_rate,omitempty"` // type=play_pcm：PCM 采样率（默认 24000）
	Speakers   int   `json:"speakers,omitempty"`    // type=voice：本模型音色总数（Piper=1、fanchen-C=187）
	// SpeakerID/Speed 用指针：0 和 0.0 在 set_voice 里分别是【合法音色号】与"不改"的区分点，
	// 用值类型会被 omitempty 吞掉——sid=0 是最常用的第一个音色，吞掉就永远选不中它。
	// sidecar 侧对 None 的语义即"该项不变"。
	SpeakerID *int     `json:"speaker_id,omitempty"` // type=voice/set_voice：音色号
	Speed     *float64 `json:"speed,omitempty"`      // type=voice/set_voice：语速
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
		// voice 不是"事件"而是状态上报（sidecar 连上时主动发一次、set_voice 后回发一次）：
		// 记下来供 Status() 用，界面据此知道本模型有几个音色可选、当前选的是哪个。
		if m.Type == "voice" {
			s.mu.Lock()
			s.voice = Voice{Speakers: m.Speakers}
			if m.SpeakerID != nil {
				s.voice.SpeakerID = *m.SpeakerID
			}
			if m.Speed != nil {
				s.voice.Speed = *m.Speed
			}
			v := s.voice
			s.mu.Unlock()
			s.log.Info("语音 sidecar 音色能力", "音色总数", v.Speakers, "当前", v.SpeakerID, "语速", v.Speed)
			s.notify() // 刷新界面状态
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
	case KindAudio:
		// realtime 上行：base64 PCM → 原始字节，转发给云端。解码失败则丢弃这一块。
		pcm, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			return Event{}, false
		}
		return Event{Kind: KindAudio, PCM: pcm}, true
	default:
		return Event{}, false
	}
}

// Speak 实现 Service：请求 sidecar 合成并播放文本。
func (s *Sidecar) Speak(ctx context.Context, text string) error {
	return s.send(ctx, sidecarMsg{Type: "speak", Text: text})
}

// SetVoice 实现 Service：换 TTS 音色/语速。sid<0 或 speed<=0 表示该项不变。
// sidecar 每次合成都带 sid/speed，故即时生效，不重载模型、不重启。
// 成功后 sidecar 会回发一条 voice 上报，Status() 里的值由那条回执更新（以 sidecar 夹紧后的
// 实际值为准，而不是我们请求的值）。
func (s *Sidecar) SetVoice(ctx context.Context, sid int, speed float64) error {
	m := sidecarMsg{Type: "set_voice"}
	if sid >= 0 {
		m.SpeakerID = &sid
	}
	if speed > 0 {
		m.Speed = &speed
	}
	return s.send(ctx, m)
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

// StreamStart 让 sidecar 开始把麦克风原始音频（16k/单声道/int16）以 audio 事件上行，
// 供 realtime 模式转发给云端语音大模型。本地 ASR 在此期间暂停。
func (s *Sidecar) StreamStart(ctx context.Context) error {
	return s.send(ctx, sidecarMsg{Type: "stream_start"})
}

// StreamStop 停止麦克风原始音频上行（恢复本地 ASR 链路）。
func (s *Sidecar) StreamStop(ctx context.Context) error {
	return s.send(ctx, sidecarMsg{Type: "stream_stop"})
}

// PlayPCM 让 sidecar 播放一段裸 PCM（realtime 云端 TTS 分片）。sampleRate 通常 24000。
// 与 Speak/PlayAudio 共用同一条串行播放线程，abort 会一并清空。
func (s *Sidecar) PlayPCM(ctx context.Context, pcm []byte, sampleRate int) error {
	return s.send(ctx, sidecarMsg{
		Type:       "play_pcm",
		Data:       base64.StdEncoding.EncodeToString(pcm),
		SampleRate: sampleRate,
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
	// coder/websocket 不允许并发写，故串行化——但【必须用独立的 writeMu，不能用 s.mu】。
	// s.mu 保护的是 conn/connected 这类瞬时状态，Status() 也要拿它；而这里的 conn.Write 是一次
	// 网络写，最坏要等满 writeTO。若两者共用一把锁，sidecar 一卡住写，Status() 就跟着卡
	// → statusSnapshot() 卡 → 每个新网页在 OnConnect 那一步就挂死、连接堆成 CLOSE_WAIT，
	// 整个界面失去响应。锁的作用域必须跟着"保护什么"走，而不是图省事复用一把。
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
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
	return Status{ASRRunning: s.connected, TTSRunning: s.connected, Detail: detail, Voice: s.voice}
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
