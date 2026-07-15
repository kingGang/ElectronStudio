// Package realtime 是 OpenAI-Realtime 协议兼容的语音对话客户端。
//
// 它把"语音进→语音出"的端到端大模型（服务端自做 VAD/ASR/LLM/TTS）抽象成一个会话：
// 上行推原始音频、下行收音频增量 + 转写 + 函数调用，并支持随时打断。国产端到端语音
// 大模型（阿里 Qwen-Omni-Realtime、智谱 GLM-Realtime）都克隆了 OpenAI-Realtime 的事件
// 格式，故一套客户端配不同 Backend 即可切换后端。首个后端是 Qwen（见 backend_qwen.go）。
//
// 与 internal/llm 的回合制 Provider 不同：这里是全双工连续音频流，不走 Chat/Complete。
package realtime

import "encoding/json"

// 客户端 → 服务端事件类型。
const (
	EvSessionUpdate      = "session.update"             // 更新会话配置（模型/音色/VAD/工具）
	EvInputAudioAppend   = "input_audio_buffer.append"  // 追加一段上行音频（base64）
	EvInputAudioCommit   = "input_audio_buffer.commit"  // 提交音频缓冲（手动 VAD 时用；服务端 VAD 下可省）
	EvInputAudioClear    = "input_audio_buffer.clear"   // 清空上行缓冲
	EvConversationCreate = "conversation.item.create"   // 插入一条会话项（文本消息 / 函数结果）
	EvResponseCreate     = "response.create"            // 触发模型生成一次回复
	EvResponseCancel     = "response.cancel"            // 打断：取消进行中的回复
)

// 服务端 → 客户端事件类型（只列我们要处理的；其余忽略）。
const (
	EvSessionCreated = "session.created" // 连接建立后服务端首发
	EvSessionUpdated = "session.updated" // session.update 的回执（回显生效配置）
	EvError          = "error"           // 错误

	// 服务端 VAD：检测到用户开始/停止说话。SpeechStarted 是【打断】的触发点。
	EvSpeechStarted = "input_audio_buffer.speech_started"
	EvSpeechStopped = "input_audio_buffer.speech_stopped"

	// 用户语音的 ASR 转写（Qwen/GLM 都放在这个事件里）。
	EvInputTranscriptionDone = "conversation.item.input_audio_transcription.completed"

	// 助手回复的音频与其对应文本。
	EvAudioDelta          = "response.audio.delta"            // 一段 PCM 音频增量（base64）
	EvAudioDone           = "response.audio.done"             // 本次回复音频结束
	EvAudioTranscriptDone = "response.audio_transcript.done"  // 助手这次说的话（完整文本）

	// 函数调用：Qwen 会流式吐 arguments，最终 .done 带齐 name/call_id/arguments。
	EvFunctionCallArgsDelta = "response.function_call_arguments.delta"
	EvFunctionCallArgsDone  = "response.function_call_arguments.done"

	EvResponseCreated = "response.created" // 一次回复开始（机器人要说话了 → 半双工：暂停上行麦克风）
	EvResponseDone    = "response.done"    // 一次回复彻底结束（恢复上行）
)

// envelope 是所有事件的公共外层：先按 Type 分派，再按需解具体字段。
type envelope struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
}

// ---- 客户端发出的事件 ----

// sessionUpdateMsg 配置会话。字段命名对齐 OpenAI-Realtime；各后端的差异（音频格式枚举值、
// 工具结构是否嵌套）由 Backend 在构造它时填入，客户端本身不关心后端。
type sessionUpdateMsg struct {
	Type    string         `json:"type"` // = EvSessionUpdate
	Session sessionConfig  `json:"session"`
}

// sessionConfig 是 session.update 里的 session 对象。Model 仅 GLM 需要（放这里）；
// Qwen 的 model 在 URL query，留空即可。Tools 用 json.RawMessage 承载——不同后端的工具
// 结构不同（Qwen 嵌套 / GLM 扁平），由 Backend 决定序列化形态，客户端不硬编码。
type sessionConfig struct {
	Model                   string          `json:"model,omitempty"`
	Modalities              []string        `json:"modalities,omitempty"`
	Voice                   string          `json:"voice,omitempty"`
	Instructions            string          `json:"instructions,omitempty"`
	InputAudioFormat        string          `json:"input_audio_format,omitempty"`
	OutputAudioFormat       string          `json:"output_audio_format,omitempty"`
	TurnDetection           any             `json:"turn_detection,omitempty"`
	InputAudioTranscription any             `json:"input_audio_transcription,omitempty"`
	Tools                   json.RawMessage `json:"tools,omitempty"`
	// 注意：【不带 tool_choice / parallel_tool_calls】——Qwen realtime 不支持，发了会崩连接。
}

// appendAudioMsg 追加一段上行音频（PCM base64）。
type appendAudioMsg struct {
	Type  string `json:"type"`  // = EvInputAudioAppend
	Audio string `json:"audio"` // base64(PCM)
}

// createItemMsg 插入一条会话项：文本消息，或函数调用结果（function_call_output）。
type createItemMsg struct {
	Type string        `json:"type"` // = EvConversationCreate
	Item conversationItem `json:"item"`
}

// conversationItem 承载两类项：普通消息（Role+Content）或函数结果（ItemType=function_call_output）。
type conversationItem struct {
	ItemType string        `json:"type"`               // "message" | "function_call_output"
	Role     string        `json:"role,omitempty"`     // 消息才有：user/assistant/system
	Content  []contentPart `json:"content,omitempty"`  // 消息内容
	CallID   string        `json:"call_id,omitempty"`  // 函数结果：与 function_call 的 call_id 对应
	Output   string        `json:"output,omitempty"`   // 函数结果：执行结果（字符串）
}

type contentPart struct {
	PartType string `json:"type"`           // input_text / input_audio / text
	Text     string `json:"text,omitempty"`
}

// simpleMsg 是只有 type 字段的事件（commit/clear/response.create/response.cancel）。
type simpleMsg struct {
	Type string `json:"type"`
}

// ---- 服务端发来的事件（按需取字段）----

// functionCallDone 对应 response.function_call_arguments.done：一次完整的函数调用请求。
type functionCallDone struct {
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"` // JSON 字符串（模型给的入参）
}

// transcriptDone 承载 助手回复文本 或 用户语音转写（两个事件字段名不同，分别取）。
type audioTranscriptDone struct {
	Transcript string `json:"transcript"`
}
type inputTranscriptionDone struct {
	ItemID     string `json:"item_id"`
	Transcript string `json:"transcript"`
}

// audioDelta 承载一段下行音频增量。
type audioDelta struct {
	Delta string `json:"delta"` // base64(PCM)
}

// errorEvent 承载服务端错误。
type errorEvent struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
