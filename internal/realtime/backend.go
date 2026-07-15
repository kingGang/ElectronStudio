package realtime

import "encoding/json"

// Backend 抽象一个具体的端到端语音后端（Qwen / GLM / …）。Client 只跟它打交道，
// 后端之间的差异（WS 地址与 model 传法、鉴权、工具结构是否嵌套、function_call_output
// 是否带 call_id、音频格式枚举值）全部收敛在这里，Client 本身与后端无关。
type Backend interface {
	// DialURL 返回要连接的完整 WebSocket 地址（Qwen 把 model 放 query；GLM 放 session）。
	DialURL() string
	// AuthHeader 返回鉴权头的值（通常是 "Bearer <key>"；GLM 可能是 JWT）。
	AuthHeader() string

	// BuildSession 构造 session.update 里的 session 对象。persona 是系统提示（人设 + 工具铁律），
	// tools 是本次要声明的工具（已按【本后端要求的结构】序列化好——见 EncodeTools）。
	BuildSession(persona string, tools json.RawMessage) sessionConfig

	// EncodeTools 把工具定义按本后端的结构序列化。Qwen 要嵌套 {type:function,function:{...}}，
	// GLM 要扁平 {type,name,description,parameters}。传 nil/空返回 nil（不声明工具）。
	EncodeTools(tools []ToolDef) (json.RawMessage, error)

	// FunctionOutputItem 构造回传函数执行结果的会话项。Qwen 用 call_id 关联；GLM 无 call_id
	// 靠顺序——差异在这里吸收，Client 统一传 callID+output 即可。
	FunctionOutputItem(callID, output string) conversationItem
}

// ToolDef 是一个函数工具的中立定义（与后端无关）。上层从现有 tools 注册表转换过来。
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ---- Client 对外抛出的事件（已剥离后端差异，供 cmd 层消费）----

// EventKind 是对外事件的种类。
type EventKind int

const (
	// KindUserTranscript：用户这句话的 ASR 文本（服务端识别的，供 UI 展示）。
	KindUserTranscript EventKind = iota
	// KindAssistantText：助手这次回复的完整文本（与音频对应，供 UI 展示 / 表情）。
	KindAssistantText
	// KindAudio：一段下行 PCM 音频增量（已 base64 解码），送设备喇叭播放。
	KindAudio
	// KindSpeechStarted：服务端 VAD 检测到用户开口 —— 打断信号（停本地播放）。
	KindSpeechStarted
	// KindResponseDone：一次回复结束（正常或被打断）。
	KindResponseDone
	// KindFunctionCall：模型请求调用工具。上层执行后须调 Client.SendFunctionResult 回传。
	KindFunctionCall
	// KindError：错误。
	KindError
	// KindResponseStarted：一次回复开始（机器人要说话了）。半双工据此暂停上行麦克风。
	KindResponseStarted
)

// Event 是 Client.Events() 抛出的对外事件。按 Kind 取对应字段。
type Event struct {
	Kind EventKind

	Text  string // KindUserTranscript / KindAssistantText / KindError
	Audio []byte // KindAudio：解码后的 PCM 字节

	// KindFunctionCall：
	CallID   string
	FuncName string
	FuncArgs string // JSON 字符串
}
