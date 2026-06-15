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

// WeatherTool 构造"查天气"工具。get 由上层注入（接天气客户端）。
func WeatherTool(get func(ctx context.Context, city string) (string, error)) Tool {
	schema := objectSchema(`{"city":{"type":"string","description":"城市名；留空则按当前位置"}}`)
	return Tool{
		Spec: Spec{Name: "get_weather", Description: "查询某城市的当前天气", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				City string `json:"city"`
			}
			_ = json.Unmarshal([]byte(args), &p)
			return get(ctx, p.City)
		},
	}
}

// ReminderTool 构造"设提醒"工具：N 分钟后提醒。add 由上层注入（接调度器）。
func ReminderTool(add func(ctx context.Context, minutes int, text string) (string, error)) Tool {
	schema := objectSchema(
		`{"minutes":{"type":"integer","description":"多少分钟后提醒"},"text":{"type":"string","description":"提醒内容"}}`,
		"minutes", "text")
	return Tool{
		Spec: Spec{Name: "set_reminder", Description: "设置一个提醒，N 分钟后机器人会说出提醒内容", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Minutes int    `json:"minutes"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Minutes <= 0 || p.Text == "" {
				return "", fmt.Errorf("需提供 minutes(>0) 与 text")
			}
			return add(ctx, p.Minutes, p.Text)
		},
	}
}

// ImageTool 构造"生成图片"工具：按文字描述生成图片（如 MiniMax 文生图）。gen 由上层注入。
func ImageTool(gen func(ctx context.Context, prompt string) (string, error)) Tool {
	schema := objectSchema(`{"prompt":{"type":"string","description":"要生成的图片内容的详细描述"}}`, "prompt")
	return Tool{
		Spec: Spec{Name: "generate_image", Description: "根据文字描述生成一张图片，并显示在机器人屏幕上", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Prompt == "" {
				return "", fmt.Errorf("需提供 prompt")
			}
			return gen(ctx, p.Prompt)
		},
	}
}

// MusicGenTool 构造"生成音乐"工具：按描述（可选歌词）用 MiniMax 生成一段音乐。gen 由上层注入。
func MusicGenTool(gen func(ctx context.Context, prompt, lyrics string) (string, error)) Tool {
	schema := objectSchema(
		`{"prompt":{"type":"string","description":"音乐风格/情绪/主题的描述，如『轻快治愈的电子乐』"},"lyrics":{"type":"string","description":"可选歌词；留空则生成纯音乐"}}`,
		"prompt")
	return Tool{
		Spec: Spec{Name: "generate_music", Description: "用 AI 生成一段音乐（可给风格描述与可选歌词），并播放出来", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Prompt string `json:"prompt"`
				Lyrics string `json:"lyrics"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Prompt == "" {
				return "", fmt.Errorf("需提供 prompt")
			}
			return gen(ctx, p.Prompt, p.Lyrics)
		},
	}
}

// MusicControlTool 构造"音乐控制"工具：对当前播放做下一首/上一首/暂停/继续/停止。
// 用户说"换一首/下一首/切歌""暂停""继续""停"时用它，而不是重新搜索播放。
func MusicControlTool(ctrl func(ctx context.Context, action string) (string, error)) Tool {
	schema := objectSchema(
		`{"action":{"type":"string","enum":["next","prev","pause","resume","stop"],"description":"next=下一首/换一首, prev=上一首, pause=暂停, resume=继续, stop=停止"}}`,
		"action")
	return Tool{
		Spec: Spec{Name: "music_control", Description: "切换/控制正在播放的音乐。用户说『换一首/换个/下一首/切歌』(没报具体歌名时)用 action=next；『上一首』=prev；『暂停』=pause；『继续』=resume；『停/别放了』=stop。换歌优先用本工具，不要用 play_music 重新搜索。", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Action string `json:"action"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Action == "" {
				return "", fmt.Errorf("需提供 action")
			}
			return ctrl(ctx, p.Action)
		},
	}
}

// MusicTool 构造"放首歌"工具：按关键词搜索并播放。play 由上层注入（接音乐服务）。
func MusicTool(play func(ctx context.Context, query string) (string, error)) Tool {
	schema := objectSchema(`{"query":{"type":"string","description":"要播放的歌曲名或歌手名"}}`, "query")
	return Tool{
		Spec: Spec{Name: "play_music", Description: "按歌名或歌手搜索并播放具体歌曲（例：『放七里香』『放周杰伦的歌』）。仅在用户报了具体歌名/歌手时用；只想『换一首/下一首』而没指定歌名时改用 music_control。", Parameters: schema},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Query == "" {
				return "", fmt.Errorf("请提供歌曲名")
			}
			return play(ctx, p.Query)
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
