package realtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestQwenDialURL 验证 model 放 query、base 末尾斜杠归一。
func TestQwenDialURL(t *testing.T) {
	q := &QwenBackend{WSBase: "wss://x.maas.aliyuncs.com/", Model: "qwen3.5-omni-flash-realtime"}
	got := q.DialURL()
	want := "wss://x.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen3.5-omni-flash-realtime"
	if got != want {
		t.Fatalf("DialURL:\n 期望 %s\n 实际 %s", want, got)
	}
	// 空 base 用默认共享端点。
	if d := (&QwenBackend{}).DialURL(); !strings.HasPrefix(d, "wss://dashscope.aliyuncs.com/") {
		t.Fatalf("默认端点错误: %s", d)
	}
	// 空 model 用默认支持工具的型号。
	if !strings.Contains((&QwenBackend{}).DialURL(), "qwen3.5-omni-flash-realtime") {
		t.Fatal("默认 model 应为 qwen3.5-omni-flash-realtime（唯一支持工具的两个之一）")
	}
}

// TestQwenToolsNested 锁死 Qwen 的工具必须是【嵌套】结构，否则会被服务端静默丢弃（血泪教训）。
func TestQwenToolsNested(t *testing.T) {
	q := &QwenBackend{}
	raw, err := q.EncodeTools([]ToolDef{{
		Name: "get_weather", Description: "查天气",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("工具不是数组: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("应有 1 个工具, 实际 %d", len(arr))
	}
	// 必须有嵌套的 function 对象，name 在 function 里，而不是顶层。
	if _, ok := arr[0]["function"]; !ok {
		t.Fatal("Qwen 工具必须嵌套在 function 对象里（扁平结构会被静默丢弃）")
	}
	var fn struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	_ = json.Unmarshal(raw[1:len(raw)-1], &fn) // 剥掉数组括号取第一个元素
	if fn.Function.Name != "get_weather" {
		t.Fatalf("name 应在 function 内, 实际解析到 %q", fn.Function.Name)
	}
	// 空工具返回 nil（不声明）。
	if r, _ := q.EncodeTools(nil); r != nil {
		t.Fatal("空工具应返回 nil")
	}
}

// TestQwenSessionNoToolChoice 锁死【绝不发 tool_choice】——发了会让 Qwen realtime 崩连接。
func TestQwenSessionNoToolChoice(t *testing.T) {
	q := &QwenBackend{}
	sess := q.BuildSession("你是小电", json.RawMessage(`[]`))
	blob, _ := json.Marshal(sessionUpdateMsg{Type: EvSessionUpdate, Session: sess})
	s := string(blob)
	if strings.Contains(s, "tool_choice") {
		t.Fatal("session.update 绝不能含 tool_choice（Qwen realtime 不支持，会 InternalError 崩连接）")
	}
	if strings.Contains(s, "parallel_tool_calls") {
		t.Fatal("session.update 绝不能含 parallel_tool_calls")
	}
	// 打断依赖 interrupt_response=true。
	if !strings.Contains(s, "interrupt_response") {
		t.Fatal("turn_detection 应含 interrupt_response（打断能力）")
	}
	// 音频格式：上行 pcm16、下行 pcm。
	if !strings.Contains(s, `"input_audio_format":"pcm16"`) || !strings.Contains(s, `"output_audio_format":"pcm"`) {
		t.Fatalf("音频格式不符: %s", s)
	}
}

// TestQwenFunctionOutputHasCallID 验证 Qwen 回传结果带 call_id。
func TestQwenFunctionOutputHasCallID(t *testing.T) {
	item := (&QwenBackend{}).FunctionOutputItem("call_abc", `{"ok":true}`)
	if item.ItemType != "function_call_output" || item.CallID != "call_abc" || item.Output != `{"ok":true}` {
		t.Fatalf("function_call_output 构造错误: %+v", item)
	}
	blob, _ := json.Marshal(item)
	if !strings.Contains(string(blob), `"call_id":"call_abc"`) {
		t.Fatalf("Qwen 回传必须带 call_id: %s", blob)
	}
}

// TestDispatch 验证读到的服务端事件被正确翻译为对外 Event（不碰网络）。
func TestDispatch(t *testing.T) {
	c := New(&QwenBackend{}, nil)
	// 用一个够大的缓冲，逐条投递，读出来核对。
	cases := []struct {
		raw  string
		kind EventKind
		want func(Event) bool
	}{
		{`{"type":"response.audio_transcript.done","transcript":"你好"}`, KindAssistantText,
			func(e Event) bool { return e.Text == "你好" }},
		{`{"type":"conversation.item.input_audio_transcription.completed","transcript":"北京天气"}`, KindUserTranscript,
			func(e Event) bool { return e.Text == "北京天气" }},
		{`{"type":"response.function_call_arguments.done","name":"get_weather","call_id":"c1","arguments":"{\"city\":\"北京\"}"}`, KindFunctionCall,
			func(e Event) bool { return e.FuncName == "get_weather" && e.CallID == "c1" && strings.Contains(e.FuncArgs, "北京") }},
		{`{"type":"input_audio_buffer.speech_started"}`, KindSpeechStarted, func(e Event) bool { return true }},
		{`{"type":"response.done"}`, KindResponseDone, func(e Event) bool { return true }},
		{`{"type":"response.audio.delta","delta":"AAAA"}`, KindAudio, func(e Event) bool { return len(e.Audio) == 3 }}, // "AAAA" b64 = 3 字节
	}
	for _, tc := range cases {
		c.dispatch([]byte(tc.raw))
		select {
		case ev := <-c.events:
			if ev.Kind != tc.kind {
				t.Fatalf("%s → kind 期望 %d 实际 %d", tc.raw, tc.kind, ev.Kind)
			}
			if !tc.want(ev) {
				t.Fatalf("%s → 事件字段不符: %+v", tc.raw, ev)
			}
		default:
			t.Fatalf("%s → 没产出事件", tc.raw)
		}
	}
	// 未知事件不产出。
	c.dispatch([]byte(`{"type":"session.updated","session":{}}`))
	select {
	case ev := <-c.events:
		t.Fatalf("未知事件不应产出, 却得到 %+v", ev)
	default:
	}
}
