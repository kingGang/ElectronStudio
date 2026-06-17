// Package xiaozhi 对接小智(xiaozhi/tenclass) AI 的 WebSocket 协议。
//
// 协议要点（与 Verdure.Assistant .NET 实现一致）：
//   - 连接头：Authorization: Bearer <token>、Protocol-Version: 1、Device-Id、Client-Id
//   - 连上后发 hello 握手，服务端回 hello 带 session_id
//   - 发文字：{"type":"listen","state":"detect","text":"..."}
//   - 收回复：{"type":"tts","state":"sentence_start","text":"..."} 多条 + state:"stop" 结束；
//     {"type":"llm","emotion":"..."} 情绪；{"type":"stt","text":"..."} 识别文本
//
// 本实现做"文字进 / 文字(+情绪)出"，语音(Opus)作为后续。直连(ws_url+token)；
// 配置了 ota_url 则先走 OTA 激活换取地址与 token。
package xiaozhi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Config 是小智连接配置。
type Config struct {
	WSURL    string
	OTAURL   string
	Token    string
	DeviceID string
	ClientID string
}

// Reply 是一次问答的结果。
type Reply struct {
	Text    string // 拼接的回复文本
	Emotion string // 最近一次情绪（如 happy/neutral），可空
	Audio   []byte // 小智自带 TTS 的音频（Ogg/Opus 容器）；空=无（用本端 TTS）
}

// Client 维护一条到小智的 WebSocket 连接。
type Client struct {
	cfg Config
	log *slog.Logger

	mu          sync.Mutex // 串行化一次问答（单连接）
	conn        *websocket.Conn
	sessionID   string
	personaSent bool // 本会话是否已注入本机角色（只在首条注入一次）
}

// New 创建小智客户端。DeviceID/ClientID 为空则自动生成。
func New(cfg Config, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = randMAC()
	}
	if cfg.ClientID == "" {
		cfg.ClientID = uuid()
	}
	return &Client{cfg: cfg, log: log}
}

// helloMsg 客户端握手。
func helloMsg() map[string]any {
	return map[string]any{
		"type": "hello", "version": 1, "transport": "websocket",
		"features":     map[string]any{"mcp": false},
		"audio_params": map[string]any{"format": "opus", "sample_rate": 16000, "channels": 1, "frame_duration": 60},
	}
}

// ensureConn 确保已连接并完成 hello 握手（调用方持锁）。
func (c *Client) ensureConn(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	wsURL := c.cfg.WSURL
	token := c.cfg.Token
	if c.cfg.OTAURL != "" {
		u, t, err := c.activate(ctx)
		if err != nil {
			return fmt.Errorf("xiaozhi: OTA 激活失败: %w", err)
		}
		if u != "" {
			wsURL = u
		}
		if t != "" {
			token = t
		}
	}
	if wsURL == "" {
		return fmt.Errorf("xiaozhi: 未配置 ws_url")
	}
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	h.Set("Protocol-Version", "1")
	h.Set("Device-Id", c.cfg.DeviceID)
	h.Set("Client-Id", c.cfg.ClientID)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return fmt.Errorf("xiaozhi: 连接失败: %w", err)
	}
	conn.SetReadLimit(8 << 20)
	// 发 hello，等服务端 hello。
	if err := writeJSON(ctx, conn, helloMsg()); err != nil {
		conn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("xiaozhi: 发送 hello 失败: %w", err)
	}
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	defer hcancel()
	for {
		var m map[string]any
		if err := readJSON(hctx, conn, &m); err != nil {
			conn.Close(websocket.StatusInternalError, "")
			return fmt.Errorf("xiaozhi: 等待服务端 hello 失败: %w", err)
		}
		if m["type"] == "hello" {
			if sid, ok := m["session_id"].(string); ok {
				c.sessionID = sid
			}
			break
		}
	}
	c.conn = conn
	c.log.Info("已连接小智", "url", wsURL, "session", c.sessionID)
	return nil
}

// activate 走 OTA：POST 设备信息，返回 websocket 地址与 token。
// 设备未在 xiaozhi.me 绑定时，服务端返回激活码，这里以错误形式带出提示。
func (c *Client) activate(ctx context.Context) (wsURL, token string, err error) {
	body, _ := json.Marshal(map[string]any{
		"application": map[string]any{"version": "1.0.0"},
		"board":       map[string]any{"type": "electronstudio", "name": "ElectronStudio"},
		"mac_address": c.cfg.DeviceID,
		"uuid":        c.cfg.ClientID,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.OTAURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Device-Id", c.cfg.DeviceID)
	req.Header.Set("Client-Id", c.cfg.ClientID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", "", err
	}
	if act, ok := raw["activation"].(map[string]any); ok {
		code, _ := act["code"].(string)
		msg, _ := act["message"].(string)
		return "", "", fmt.Errorf("设备未激活，请在 xiaozhi.me 输入验证码 %s 绑定（%s）", code, msg)
	}
	if ws, ok := raw["websocket"].(map[string]any); ok {
		wsURL, _ = ws["url"].(string)
		token, _ = ws["token"].(string)
	}
	return wsURL, token, nil
}

// Ask 发送一句文字，收集完整回复（文本/情绪/音频）后一次性返回（非流式）。
func (c *Client) Ask(ctx context.Context, text, persona string) (Reply, error) {
	return c.AskStream(ctx, text, persona, nil, nil)
}

// AskStream 发送一句文字并流式回调：
//   - onText：每收到一段 sentence_start 文本即调用一次（降低首字延迟）；
//   - onAudio：每说完一句即把该句的 Opus 帧封成一段独立 Ogg/Opus 回调一次（边收边播，降低首声延迟）。
//
// 当 onAudio 非空时，音频走流式回调、不再在返回值里给整段 Audio（避免重复播放）；
// onAudio 为空时，音频收齐后随 Reply.Audio 一次性返回（供非流式/工具路径）。
// persona 非空且本会话首条时注入本机角色设定（否则用小智服务端自带角色）。
func (c *Client) AskStream(ctx context.Context, text, persona string, onText func(string), onAudio func([]byte)) (Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureConn(ctx); err != nil {
		return Reply{}, err
	}
	send := text
	if persona != "" && !c.personaSent {
		send = "请始终扮演以下角色与我对话：" + persona + "\n\n" + text
		c.personaSent = true
	}
	req := map[string]any{"type": "listen", "state": "detect", "text": send}
	if c.sessionID != "" {
		req["session_id"] = c.sessionID
	}
	if err := writeJSON(ctx, c.conn, req); err != nil {
		c.drop()
		return Reply{}, fmt.Errorf("xiaozhi: 发送失败: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var sb strings.Builder
	var emotion string
	var sentFrames [][]byte // 当前句的 Opus 帧（流式：每句一段 Ogg）
	var allFrames [][]byte  // 全部帧（非流式：收齐后整段返回）
	// flushSentence 把当前句的帧封成一段 Ogg 经 onAudio 送出，然后清空。
	flushSentence := func() {
		if onAudio != nil && len(sentFrames) > 0 {
			onAudio(muxOggOpus(sentFrames, 0)) // 分段用 pre-skip=0，避免砍掉每句开头
		}
		sentFrames = sentFrames[:0]
	}
	finalAudio := func() []byte {
		if onAudio != nil {
			return nil // 已流式送出
		}
		return muxOggOpus(allFrames, 3840) // 完整流用编码器默认 pre-skip
	}
	for {
		typ, data, err := c.conn.Read(rctx)
		if err != nil {
			c.drop()
			flushSentence()
			if sb.Len() > 0 {
				return Reply{Text: sb.String(), Emotion: emotion, Audio: finalAudio()}, nil
			}
			return Reply{}, fmt.Errorf("xiaozhi: 读取回复失败: %w", err)
		}
		if typ == websocket.MessageBinary {
			cp := append([]byte(nil), data...) // 拷贝：data 复用底层缓冲
			sentFrames = append(sentFrames, cp)
			if onAudio == nil {
				allFrames = append(allFrames, cp)
			}
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m["type"] {
		case "llm":
			if e, ok := m["emotion"].(string); ok && e != "" {
				emotion = e
			}
		case "tts":
			switch m["state"] {
			case "sentence_start":
				flushSentence() // 先把上一句的音频送出，再开始下一句
				if t, ok := m["text"].(string); ok && t != "" {
					sb.WriteString(t)
					if onText != nil {
						onText(t) // 逐句即时回调，首字更快
					}
				}
			case "stop":
				flushSentence()
				return Reply{Text: strings.TrimSpace(sb.String()), Emotion: emotion, Audio: finalAudio()}, nil
			}
		}
	}
}

// Close 关闭连接。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
}

func (c *Client) drop() {
	if c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "")
		c.conn = nil
		c.sessionID = ""
		c.personaSent = false
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

func readJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// NewDeviceID 生成一个随机 MAC 形式的设备 ID（供上层首次生成后持久化到配置，保持稳定）。
func NewDeviceID() string { return randMAC() }

// NewClientID 生成一个随机 UUID 作为客户端 ID（供上层首次生成后持久化到配置）。
func NewClientID() string { return uuid() }

func randMAC() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
