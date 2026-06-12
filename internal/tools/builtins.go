package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 本文件提供一组内置工具的构造器。需要副作用（情绪/动作/设备）的工具以闭包注入，
// 保持 tools 包与业务解耦。

// EmotionTool 构造"设置情绪"工具。set 在执行时被调用以真正改变情绪。
func EmotionTool(emotions []string, set func(emotion string) error) Tool {
	schema := objectSchema(fmt.Sprintf(
		`{"emotion":{"type":"string","enum":[%s],"description":"目标情绪"}}`, jsonEnum(emotions)),
		"emotion")
	return Tool{
		Spec: Spec{Name: "set_emotion", Description: "设置机器人的情绪表情", Parameters: schema},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Emotion string `json:"emotion"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if err := set(p.Emotion); err != nil {
				return "", err
			}
			return "情绪已设置为 " + p.Emotion, nil
		},
	}
}

// ActionTool 构造"播放动作"工具。names 用于在 schema 中枚举可选动作。
func ActionTool(names []string, play func(name string) error) Tool {
	schema := objectSchema(fmt.Sprintf(
		`{"name":{"type":"string","enum":[%s],"description":"动作名"}}`, jsonEnum(names)),
		"name")
	return Tool{
		Spec: Spec{Name: "play_action", Description: "让机器人播放一个预定义动作", Parameters: schema},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if err := play(p.Name); err != nil {
				return "", err
			}
			return "已播放动作 " + p.Name, nil
		},
	}
}

// LookTool 构造"看一眼"工具：用摄像头抓一帧交给视觉模型描述/回答。
//   - capture 抓取当前画面并返回 JPEG 字节；
//   - describe 把图片 + 问题交给视觉模型，返回描述文本。
// 二者由上层注入（cmd 接摄像头 + LLM 视觉）。
func LookTool(
	capture func(ctx context.Context) ([]byte, error),
	describe func(ctx context.Context, jpeg []byte, question string) (string, error),
) Tool {
	schema := objectSchema(`{"question":{"type":"string","description":"关于画面的问题；留空则描述所见"}}`)
	return Tool{
		Spec: Spec{Name: "look", Description: "用机器人的摄像头看一眼，描述画面或回答关于画面的问题", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal([]byte(args), &p)
			if p.Question == "" {
				p.Question = "请简要描述你看到的画面"
			}
			jpeg, err := capture(ctx)
			if err != nil {
				return "", err
			}
			return describe(ctx, jpeg, p.Question)
		},
	}
}

// TimeTool 构造"获取当前时间"工具（无副作用，演示信息类工具）。
func TimeTool() Tool {
	return Tool{
		Spec: Spec{Name: "get_time", Description: "获取当前日期与时间", Parameters: objectSchema("{}")},
		Handler: func(_ context.Context, _ string) (string, error) {
			return time.Now().Format("2006-01-02 15:04:05"), nil
		},
	}
}

// Lamp 是一个自包含的有状态设备示例（智能台灯），演示"设备控制"类工具。
type Lamp struct {
	mu sync.Mutex
	on bool
}

// NewLamp 创建一个台灯设备。
func NewLamp() *Lamp { return &Lamp{} }

// On 返回台灯当前是否开启（供外部读取/广播状态）。
func (l *Lamp) On() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.on
}

// Tool 返回控制台灯开关的工具。
func (l *Lamp) Tool() Tool {
	schema := objectSchema(`{"on":{"type":"boolean","description":"true=开灯, false=关灯"}}`, "on")
	return Tool{
		Spec: Spec{Name: "set_lamp", Description: "打开或关闭智能台灯", Parameters: schema},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				On bool `json:"on"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			l.mu.Lock()
			l.on = p.On
			l.mu.Unlock()
			if p.On {
				return "台灯已打开", nil
			}
			return "台灯已关闭", nil
		},
	}
}

// ---- JSON Schema 小工具 ----

// objectSchema 用给定的 properties 片段与必填字段拼出一个 object 类型的 JSON Schema。
func objectSchema(propsJSON string, required ...string) json.RawMessage {
	req := ""
	if len(required) > 0 {
		req = fmt.Sprintf(`,"required":[%s]`, jsonEnum(required))
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":%s%s}`, propsJSON, req))
}

// jsonEnum 把字符串切片拼成 JSON 字符串数组的内容（不含外层方括号）。
func jsonEnum(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		b, _ := json.Marshal(s)
		quoted[i] = string(b)
	}
	return strings.Join(quoted, ",")
}
