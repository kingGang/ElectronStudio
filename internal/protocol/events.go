package protocol

// 本文件定义【服务端 → 客户端】方向的事件负载，以及前后端共享的枚举类型。
// 每个结构都实现 Payload.Type()，与 protocol.go 中的 Type 常量一一对应。

// ---------------------------------------------------------------------------
// 共享枚举
// ---------------------------------------------------------------------------

// VoiceState 表示语音对话状态机的当前状态，驱动前端机器人区的视觉表现。
type VoiceState string

const (
	VoiceIdle       VoiceState = "idle"       // 待命
	VoiceConnecting VoiceState = "connecting" // 连接中
	VoiceListening  VoiceState = "listening"  // 聆听用户说话
	VoiceThinking   VoiceState = "thinking"   // 已识别, 等待大模型回应
	VoiceSpeaking   VoiceState = "speaking"   // 正在播放 TTS 回应
)

// ChatRole 表示一条对话消息的角色。
type ChatRole string

const (
	RoleUser      ChatRole = "user"
	RoleAssistant ChatRole = "assistant"
	RoleSystem    ChatRole = "system"
)

// ChatStatus 表示一条对话消息的生成状态，支持流式增量更新。
type ChatStatus string

const (
	ChatStreaming ChatStatus = "streaming" // 流式生成中, content 为「截至目前的完整文本」
	ChatFinal     ChatStatus = "final"     // 本条消息已完成
)

// Emotion 是机器人可表达的情绪，对应一段表情画面与（可选）动作编排。
type Emotion string

const (
	EmotionNeutral   Emotion = "neutral"
	EmotionHappy     Emotion = "happy"
	EmotionSad       Emotion = "sad"
	EmotionAngry     Emotion = "angry"
	EmotionSurprised Emotion = "surprised"
	EmotionConfused  Emotion = "confused"
)

// JointCount 是 ElectronBot 的舵机数量（6 轴）。集中定义以便各处引用。
const JointCount = 6

// ---------------------------------------------------------------------------
// status —— 状态快照
// ---------------------------------------------------------------------------

// RobotStatus 描述机器人 USB 连接状态。
type RobotStatus struct {
	Connected bool   `json:"connected"`
	VID       uint16 `json:"vid"` // USB 厂商 ID（ElectronBot = 0x1001）
	PID       uint16 `json:"pid"` // USB 产品 ID（ElectronBot = 0x8023）
	FPS       int    `json:"fps"` // 当前画面推送帧率
}

// ServiceStatus 描述一个 sidecar 子服务（ASR / TTS）的运行状态。
type ServiceStatus struct {
	Running bool   `json:"running"`
	Detail  string `json:"detail,omitempty"` // 附加信息, 如模型名或错误原因
}

// ModelInfo 描述一个可用的大模型。
type ModelInfo struct {
	ID       string `json:"id"`       // 唯一标识, 用于 SelectModelCommand
	Name     string `json:"name"`     // 展示名
	Provider string `json:"provider"` // 提供方, 如 ollama / openai / anthropic
}

// LLMStatus 描述大模型的当前选择与可用列表。
type LLMStatus struct {
	Active    string      `json:"active"`    // 当前生效的模型 ID
	Available []ModelInfo `json:"available"` // 可切换的全部模型
}

// StatusEvent 是各子系统状态的一次性快照，通常在连接建立或状态变化时下发。
type StatusEvent struct {
	Robot   RobotStatus   `json:"robot"`
	ASR     ServiceStatus `json:"asr"`
	TTS     ServiceStatus `json:"tts"`
	LLM     LLMStatus     `json:"llm"`
	Actions []string      `json:"actions,omitempty"` // 可用的编排动作名（供动作编排页使用）
}

// Type 实现 Payload。
func (StatusEvent) Type() Type { return TypeStatus }

// ---------------------------------------------------------------------------
// voice_state —— 语音状态机
// ---------------------------------------------------------------------------

// VoiceStateEvent 表示语音状态发生变化。
type VoiceStateEvent struct {
	State  VoiceState `json:"state"`
	Detail string     `json:"detail,omitempty"` // 可选: 变化原因, 便于调试
}

// Type 实现 Payload。
func (VoiceStateEvent) Type() Type { return TypeVoiceState }

// ---------------------------------------------------------------------------
// vad —— 语音活动检测
// ---------------------------------------------------------------------------

// VADEvent 携带语音活动信息，用于驱动前端实时波形与打断判断。
type VADEvent struct {
	Speaking bool    `json:"speaking"`        // 当前是否检测到人声
	Level    float32 `json:"level,omitempty"` // 归一化电平 [0,1], 供波形显示
}

// Type 实现 Payload。
func (VADEvent) Type() Type { return TypeVAD }

// ---------------------------------------------------------------------------
// wake —— 唤醒词
// ---------------------------------------------------------------------------

// WakeEvent 表示检测到唤醒词。
type WakeEvent struct {
	Keyword string `json:"keyword"` // 命中的唤醒词, 如「你好小电」
}

// Type 实现 Payload。
func (WakeEvent) Type() Type { return TypeWake }

// ---------------------------------------------------------------------------
// asr —— 语音识别
// ---------------------------------------------------------------------------

// ASREvent 携带语音识别结果，可多次下发中间态、最后一次 Final=true。
type ASREvent struct {
	Text  string `json:"text"`
	Final bool   `json:"final"` // true 表示这是本句的最终识别结果
}

// Type 实现 Payload。
func (ASREvent) Type() Type { return TypeASR }

// ---------------------------------------------------------------------------
// chat —— 对话消息
// ---------------------------------------------------------------------------

// ToolCall 描述大模型发起的一次工具（MCP/设备）调用及其结果。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`                // 工具名, 如 lamp.turn_on
	Arguments string `json:"arguments,omitempty"` // 入参（JSON 字符串）
	Result    string `json:"result,omitempty"`    // 执行结果（JSON 或文本）
	Status    string `json:"status,omitempty"`    // pending | ok | error
}

// ChatEvent 表示一条对话消息。前端按 ID 去重/更新：
// 流式生成期间多次下发同一 ID、Status=streaming 且 Content 为累计全文，
// 完成时下发 Status=final。
type ChatEvent struct {
	ID      string     `json:"id"`
	Role    ChatRole   `json:"role"`
	Content string     `json:"content"`
	Tools   []ToolCall `json:"tools,omitempty"`
	Status  ChatStatus `json:"status"`
}

// Type 实现 Payload。
func (ChatEvent) Type() Type { return TypeChat }

// ---------------------------------------------------------------------------
// tts —— 语音合成
// ---------------------------------------------------------------------------

// TTSState 表示语音合成播放的阶段。
type TTSState string

const (
	TTSStart   TTSState = "start"   // 开始合成/播放
	TTSPlaying TTSState = "playing" // 播放中（可携带当前句）
	TTSStop    TTSState = "stop"    // 播放结束或被打断
)

// TTSEvent 表示语音合成播放状态变化。
type TTSEvent struct {
	State      TTSState `json:"state"`
	Text       string   `json:"text,omitempty"`        // 当前正在/即将播放的文本
	SentenceID int      `json:"sentence_id,omitempty"` // 多句播放时的句序号
}

// Type 实现 Payload。
func (TTSEvent) Type() Type { return TypeTTS }

// ---------------------------------------------------------------------------
// emotion —— 情绪
// ---------------------------------------------------------------------------

// EmotionEvent 表示机器人当前情绪发生变化。
type EmotionEvent struct {
	Emotion Emotion `json:"emotion"`
}

// Type 实现 Payload。
func (EmotionEvent) Type() Type { return TypeEmotion }

// ---------------------------------------------------------------------------
// joints —— 舵机角度反馈
// ---------------------------------------------------------------------------

// JointsEvent 携带从机器人读回的 6 轴舵机真实角度（度）。
type JointsEvent struct {
	Angles  [JointCount]float32 `json:"angles"`
	Enabled bool                `json:"enabled"` // 舵机当前是否使能
}

// Type 实现 Payload。
func (JointsEvent) Type() Type { return TypeJoints }

// ---------------------------------------------------------------------------
// error / log —— 错误与日志
// ---------------------------------------------------------------------------

// ErrorEvent 通知前端发生了一个面向用户的错误。
type ErrorEvent struct {
	Code    string `json:"code"`    // 机器可读的错误码, 如 usb_disconnected
	Message string `json:"message"` // 面向用户的可读说明
}

// Type 实现 Payload。
func (ErrorEvent) Type() Type { return TypeError }

// LogEvent 向前端调试面板推送一条日志（可选功能）。
type LogEvent struct {
	Level   string `json:"level"` // debug | info | warn | error
	Message string `json:"message"`
}

// Type 实现 Payload。
func (LogEvent) Type() Type { return TypeLog }
