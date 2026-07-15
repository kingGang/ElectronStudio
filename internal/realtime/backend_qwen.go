package realtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

// QwenBackend 是阿里 Qwen-Omni-Realtime（DashScope / 百炼 MaaS）后端。
//
// 规格全部【真机实测钉死】（2026-07-15，见 docs/REALTIME.md）：
//   - model 必须是 qwen3.5-omni-flash-realtime 或 qwen3.5-omni-plus-realtime——只有这两个支持
//     function calling。qwen3-omni-flash / qwen-omni-turbo 会【静默丢弃 tools】。
//   - model 放 URL query（?model=）；鉴权 Authorization: Bearer <key>。
//   - tools 结构【嵌套】：{type:"function", function:{name,description,parameters}}。
//   - 【绝不发 tool_choice / parallel_tool_calls】——realtime 不支持，发了 MaaS 端点 InternalError 崩连接。
//   - function_call_output 用 call_id 关联。
//   - 上行 pcm16(16k)、下行 pcm(24k)。
type QwenBackend struct {
	WSBase string // 如 wss://<workspace>.cn-beijing.maas.aliyuncs.com  或  wss://dashscope.aliyuncs.com
	Model  string // qwen3.5-omni-flash-realtime（默认）/ -plus-realtime
	APIKey string
	Voice  string // 音色，如 Chelsie；空用默认
}

// 默认取值。
const (
	qwenDefaultModel = "qwen3.5-omni-flash-realtime"
	qwenDefaultBase  = "wss://dashscope.aliyuncs.com"
)

func (q *QwenBackend) model() string {
	if q.Model != "" {
		return q.Model
	}
	return qwenDefaultModel
}

// DialURL：model 放 query。WSBase 允许带或不带末尾斜杠。
func (q *QwenBackend) DialURL() string {
	base := q.WSBase
	if base == "" {
		base = qwenDefaultBase
	}
	base = strings.TrimRight(base, "/")
	return fmt.Sprintf("%s/api-ws/v1/realtime?model=%s", base, q.model())
}

func (q *QwenBackend) AuthHeader() string { return "Bearer " + q.APIKey }

// BuildSession：pcm16 上/pcm 下、server_vad（开启 interrupt_response 支持打断）、
// 内置 ASR 转写。不含 tool_choice。
func (q *QwenBackend) BuildSession(persona string, tools json.RawMessage) sessionConfig {
	// Voice 留空则【不下发】voice 字段，让服务端用该部署的默认音色——不同 MaaS 专属部署
	// 支持的音色不同（实测某部署不认 Chelsie），写死一个音色会 400。要指定音色需先确认可用值。
	return sessionConfig{
		Modalities:        []string{"text", "audio"},
		Voice:             q.Voice,
		Instructions:      persona,
		InputAudioFormat:  "pcm16",
		OutputAudioFormat: "pcm",
		TurnDetection: map[string]any{
			"type": "server_vad",
			// threshold 提高到 0.6：设备麦会收录环境音/远处人声，阈值低了会被云端 VAD 误判成用户说话。
			// 调高让它只对够响的近场人声起反应，减少误触发。silence 也加长，避免半句被切。
			"threshold":           0.6,
			"silence_duration_ms": 1000,
			"create_response":     true, // 用户停顿后自动生成回复
			"interrupt_response":  true, // 用户插话自动打断上一条
		},
		InputAudioTranscription: map[string]any{"model": "qwen3-asr-flash-realtime"},
		Tools:                   tools,
	}
}

// EncodeTools：Qwen 要【嵌套】结构 {type:"function", function:{...}}。
func (q *QwenBackend) EncodeTools(tools []ToolDef) (json.RawMessage, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	type qwenFn struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type qwenTool struct {
		Type     string `json:"type"`
		Function qwenFn `json:"function"`
	}
	out := make([]qwenTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, qwenTool{Type: "function", Function: qwenFn{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}})
	}
	return json.Marshal(out)
}

// FunctionOutputItem：Qwen 用 call_id 关联结果。
func (q *QwenBackend) FunctionOutputItem(callID, output string) conversationItem {
	return conversationItem{ItemType: "function_call_output", CallID: callID, Output: output}
}
