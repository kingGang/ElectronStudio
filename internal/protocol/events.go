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
	Connected bool `json:"connected"`
	Stuck     bool `json:"stuck,omitempty"` // 已连接但持续无就绪包(疑似固件卡死)
	// Recovering：卡死后正在【自动串口软复位(免拔电源)】自救中。为 true 时前端应显示"自动复位中…"
	// 而非"请断电"；stuck && !recovering 才是"自动复位无效、需手动断电复位"。
	Recovering bool   `json:"recovering,omitempty"`
	Speed      string `json:"speed,omitempty"` // USB 连接速度，如 "USB 2.0"/"USB 3.0"（Mock 为空）
	VID       uint16 `json:"vid"`             // USB 厂商 ID（ElectronBot = 0x1001）
	PID       uint16 `json:"pid"`             // USB 产品 ID（ElectronBot = 0x8023）
	FPS       int    `json:"fps"`             // 当前画面推送帧率
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
	Camera   bool         `json:"camera"`              // 是否配置了摄像头（前端据此显示切换按钮）
	CameraOn bool         `json:"camera_on"`           // 摄像头当前是否开启（前端据此同步开关；不省略，false 也要明确上报）
	IO       IOStatus       `json:"io"`       // I/O 路由当前配置（供设置页展示/编辑）
	Realtime RealtimeStatus `json:"realtime"` // 实时语音对话当前配置（供设置页展示/编辑）
	Music    MusicStatus    `json:"music"`    // 音乐子系统状态（音源等，供前端展示）
	Persona       string  `json:"persona,omitempty"`        // 设备角色/人设（供设置页展示/编辑）
	PersonaSource string  `json:"persona_source,omitempty"` // local | model（角色来源：本机/模型自带）
	Voice         string  `json:"voice,omitempty"`          // 声音音色（供设置页展示/编辑）
}

// MusicStatus 是音乐子系统的当前配置（供前端展示来源）。
type MusicStatus struct {
	Source   string `json:"source"`              // qq | kuwo
	LoggedIn bool   `json:"logged_in,omitempty"` // QQ 音乐是否已登录（音源为 qq 且有 cookie）
}

// IOStatus 是 I/O 路由的当前生效配置（供前端设置页显示与编辑）。
type IOStatus struct {
	AudioIn      string `json:"audio_in"`      // device | page | off
	AudioOut     string `json:"audio_out"`     // device | page | both | off
	TTSEngine    string `json:"tts_engine"`    // minimax | sidecar
	ImageOut     string `json:"image_out"`     // device | page | both | off
	DeviceVolume int    `json:"device_volume"` // 设备扬声器音量 0~100
	ServoEnable  bool   `json:"servo_enable"`  // 舵机总开关：false 时不上扭矩（可手动摆姿）
}

// RealtimeStatus 是实时语音对话的当前配置（供设置页显示与编辑）。
// 【不回传 API key 明文】——只用 HasKey 报告是否已配置，避免密钥经 WebSocket 外泄。
type RealtimeStatus struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider,omitempty"`
	WSBase   string `json:"ws_base,omitempty"`
	Model    string `json:"model,omitempty"`
	Voice    string `json:"voice,omitempty"`
	HasKey   bool   `json:"has_key"` // 是否已配置 API key（不回传明文）
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
	Images  []string   `json:"images,omitempty"` // 可选：随消息展示的图片 URL（如 MiniMax 生成图）
	Audio   string     `json:"audio,omitempty"`  // 可选：随消息展示的音频播放器 URL（如 MiniMax 生成的音乐）
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

// ScheduleJob 是一个定时任务的展示信息。
type ScheduleJob struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	At    string `json:"at,omitempty"`
	Every string `json:"every,omitempty"`
	Daily string `json:"daily,omitempty"`
	Kind  string `json:"kind"`
	Text  string `json:"text,omitempty"`
}

// ScheduleListEvent 是定时任务列表（增删后广播）。
type ScheduleListEvent struct {
	Jobs []ScheduleJob `json:"jobs"`
}

// Type 实现 Payload。
func (ScheduleListEvent) Type() Type { return TypeScheduleList }

// AudioEvent 把一段合成语音/音频以 base64 推给页面播放（调试镜像；设备侧另经 mpg123 播放）。
// Stop=true 表示打断：让页面停止当前播放（此时 Data 为空）。
type AudioEvent struct {
	Format string `json:"format,omitempty"` // mp3 | wav ...
	Data   string `json:"data,omitempty"`   // base64 编码的音频字节（小段语音用）
	URL    string `json:"url,omitempty"`    // 或给一个 HTTP 取回地址（较大音频如音乐用）
	Text   string `json:"text,omitempty"`
	Stop   bool   `json:"stop,omitempty"`   // true=停止页面当前播放（barge-in）
	Stream bool   `json:"stream,omitempty"` // true=流式分段语音：页面按到达顺序排队顺序播放（小智逐句音频）
}

// Type 实现 Payload。
func (AudioEvent) Type() Type { return TypeAudio }

// MaterialInfo 描述一段屏幕表情素材（供素材管理界面展示）。
type MaterialInfo struct {
	Name   string `json:"name"`   // 情绪名（= 素材文件名）
	Frames int    `json:"frames"` // 帧数
	FPS    int    `json:"fps"`    // 播放帧率
	Kind   string `json:"kind"`   // 素材来源：gif | frames | atlas
}

// MaterialsEvent 是屏幕表情素材列表（上传/删除后广播，连接建立时也推送一次）。
type MaterialsEvent struct {
	Materials []MaterialInfo `json:"materials"`
}

// Type 实现 Payload。
func (MaterialsEvent) Type() Type { return TypeMaterials }

// MusicEvent 表示音乐播放状态变化。
type MusicEvent struct {
	State    string  `json:"state"`              // playing | paused | stopped
	Name     string  `json:"name,omitempty"`     // 当前曲名
	Artist   string  `json:"artist,omitempty"`   // 当前歌手
	URL      string  `json:"url,omitempty"`      // 可播放流地址（供页面在 audio_out=page/both 时浏览器播放）
	Position float64 `json:"position,omitempty"` // 起播进度(秒)：刷新/重连恢复时让页面 seek 到该位置
	Restore  bool    `json:"restore,omitempty"`  // true=这是重连后的状态恢复（页面据此 seek 并按原状态播/停）
}

// Type 实现 Payload。
func (MusicEvent) Type() Type { return TypeMusicState }

// GestureEvent 表示识别到一个手势（来自手势 sidecar）。
type GestureEvent struct {
	Name       string  `json:"name"`
	Confidence float32 `json:"confidence,omitempty"`
}

// Type 实现 Payload。
func (GestureEvent) Type() Type { return TypeGesture }

// LogEvent 向前端调试面板推送一条日志（可选功能）。
type LogEvent struct {
	Level   string `json:"level"` // debug | info | warn | error
	Message string `json:"message"`
}

// Type 实现 Payload。
func (LogEvent) Type() Type { return TypeLog }
