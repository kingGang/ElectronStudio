// Command electronstudio 是 ElectronStudio 的最小可运行入口。
//
// 它把已完成的各模块接成一个闭环：
//
//	WebSocket(server) ── 入站命令 ──► dispatch ──► llm / choreography / robot(mock)
//	        ▲                                              │
//	        └────────────── 事件广播(status/chat/...) ◄────┘
//
// 当前使用 Mock 机器人与本地 Echo 模型，因此【无需真机、无需联网、无需 C 工具链】
// 即可启动：运行后用浏览器或 wscat 连接 ws://localhost:8080/ws 即可看到事件流动。
//
// 通过环境变量可挂接真实大模型（仍是纯 HTTP，无 cgo）：
//
//	OPENAI_BASE_URL  例如 https://api.openai.com/v1 或 http://localhost:11434/v1
//	OPENAI_API_KEY   鉴权密钥（本地 Ollama 可留空）
//	OPENAI_MODEL     模型名，例如 gpt-4o / qwen2.5
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"

	"github.com/kingGang/ElectronStudio/internal/choreography"
	"github.com/kingGang/ElectronStudio/internal/llm"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/robot"
	"github.com/kingGang/ElectronStudio/internal/server"
	"github.com/kingGang/ElectronStudio/internal/speech"
	"github.com/kingGang/ElectronStudio/web"
)

// systemPrompt 是对话的系统提示，约束助手扮演桌面机器人小电。
const systemPrompt = "你是桌面机器人小电，回答简洁友好。"

// maxHistory 限制保留的对话轮数，防止上下文无限增长。
const maxHistory = 20

func main() {
	addr := flag.String("addr", ":8080", "HTTP/WebSocket 监听地址")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	a, err := newApp(log)
	if err != nil {
		log.Error("初始化失败", "err", err)
		os.Exit(1)
	}
	defer a.bot.Close()

	// Ctrl+C 触发优雅关闭。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 启动语音服务（Mock 为空操作；Sidecar 会连接外部进程）。
	if err := a.speech.Start(ctx); err != nil {
		log.Warn("语音服务启动失败，将仅支持文本输入", "err", err)
	}
	defer a.speech.Close()

	// 消费入站命令与上行语音事件。
	go a.dispatchLoop(ctx)
	go a.speechLoop(ctx)

	if err := a.srv.Serve(ctx, *addr); err != nil {
		log.Error("服务退出", "err", err)
		os.Exit(1)
	}
}

// app 持有各模块依赖与少量运行时状态，并实现命令分发。
type app struct {
	srv    *server.Server
	chor   *choreography.Engine
	llm    *llm.Router
	bot    robot.Transport
	speech speech.Service
	log    *slog.Logger

	histMu  sync.Mutex
	history []llm.Message  // 对话历史（含系统提示）
	msgSeq  atomic.Uint64  // 对话消息 ID 自增源
}

// newApp 组装所有依赖。
func newApp(log *slog.Logger) (*app, error) {
	// 机器人：当前用 Mock；后续替换为 electronbot(USB) 实现即可，其余代码不变。
	bot := robot.NewMock(log)
	if err := bot.Connect(); err != nil {
		return nil, fmt.Errorf("连接机器人失败: %w", err)
	}

	// 动作编排：注册内置动作。
	chor := choreography.NewEngine(bot, choreography.WithLogger(log))
	for _, act := range choreography.DefaultActions() {
		chor.Register(act)
	}

	// 大模型：本地 Echo 兜底，确保无网络也能跑通；若配置了环境变量则追加真实模型。
	router := llm.NewRouter()
	router.Add(llm.NewEcho("echo", "本地回声"))
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		router.Add(llm.NewOpenAICompat(llm.OpenAIConfig{
			BaseURL: base,
			APIKey:  os.Getenv("OPENAI_API_KEY"),
			Model:   os.Getenv("OPENAI_MODEL"),
		}))
		log.Info("已挂接 OpenAI 兼容模型", "base", base, "model", os.Getenv("OPENAI_MODEL"))
	}

	// 语音：默认 Mock（无 sidecar 也能跑文本链路）；若配置了 sidecar 地址则对接真实语音服务。
	var voice speech.Service
	if url := os.Getenv("SPEECH_SIDECAR_URL"); url != "" {
		voice = speech.NewSidecar(url, log)
		log.Info("已配置语音 sidecar", "url", url)
	} else {
		voice = speech.NewMock(log)
	}

	a := &app{
		chor:    chor,
		llm:     router,
		bot:     bot,
		speech:  voice,
		log:     log,
		history: []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}},
	}

	// 新客户端连上时，推送一次状态快照，让前端立即渲染。
	a.srv = server.New(server.Options{
		Logger:   log,
		StaticFS: web.FS(), // 内嵌前端单页应用，访问 http://addr/ 即是界面
		OnConnect: func(c *server.Client) {
			if err := c.Send(a.statusSnapshot()); err != nil {
				log.Warn("推送初始状态失败", "err", err)
			}
		},
	})
	return a, nil
}

// dispatchLoop 持续消费入站命令，直到 ctx 取消。
func (a *app) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case in := <-a.srv.Inbound():
			a.handle(ctx, in)
		}
	}
}

// speechLoop 消费上行语音事件（唤醒 / VAD / ASR），把它们接到对话与状态广播上。
func (a *app) speechLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-a.speech.Events():
			if !ok {
				return // 事件流已关闭
			}
			a.handleSpeechEvent(ctx, ev)
		}
	}
}

// handleSpeechEvent 处理单条语音事件。
func (a *app) handleSpeechEvent(ctx context.Context, ev speech.Event) {
	switch ev.Kind {
	case speech.KindWake:
		a.srv.Broadcast(protocol.WakeEvent{Keyword: ev.Keyword})
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceListening})

	case speech.KindVAD:
		a.srv.Broadcast(protocol.VADEvent{Speaking: ev.Speaking, Level: ev.Level})

	case speech.KindASR:
		// 把识别文本作为前端的中间/最终 ASR 展示。
		a.srv.Broadcast(protocol.ASREvent{Text: ev.Text, Final: ev.Final})
		// 最终结果触发一次对话（与文本输入同一路径）。
		if ev.Final && ev.Text != "" {
			go a.handleChat(ctx, ev.Text)
		}
	}
}

// handle 按类型分发单条入站命令。
func (a *app) handle(ctx context.Context, in server.Inbound) {
	switch in.Env.Type {
	case protocol.TypeSendText:
		if cmd, err := protocol.As[protocol.SendTextCommand](in.Env); err == nil {
			go a.handleChat(ctx, cmd.Text) // 对话耗时，放后台，避免阻塞分发循环
		}

	case protocol.TypePlayAction:
		if cmd, err := protocol.As[protocol.PlayActionCommand](in.Env); err == nil {
			if err := a.chor.Play(ctx, cmd.Name, cmd.Loops); err != nil {
				a.log.Warn("播放动作失败", "name", cmd.Name, "err", err)
			}
		}

	case protocol.TypeSetEmotion:
		if cmd, err := protocol.As[protocol.SetEmotionCommand](in.Env); err == nil {
			a.srv.Broadcast(protocol.EmotionEvent{Emotion: cmd.Emotion})
			// 若存在与该情绪同名的动作，顺带播放。
			if _, ok := a.chor.Lookup(string(cmd.Emotion)); ok {
				_ = a.chor.Play(ctx, string(cmd.Emotion), 1)
			}
		}

	case protocol.TypeSelectModel:
		if cmd, err := protocol.As[protocol.SelectModelCommand](in.Env); err == nil {
			if err := a.llm.SetActive(cmd.ID); err != nil {
				a.log.Warn("切换模型失败", "id", cmd.ID, "err", err)
			}
			a.srv.Broadcast(a.statusSnapshot()) // 让所有端同步当前模型
		}

	case protocol.TypeJogJoint:
		if cmd, err := protocol.As[protocol.JogJointCommand](in.Env); err == nil {
			a.handleJog(cmd)
		}

	case protocol.TypeMic:
		if cmd, err := protocol.As[protocol.MicCommand](in.Env); err == nil {
			state := protocol.VoiceIdle
			if cmd.Action == protocol.MicStart {
				state = protocol.VoiceListening
			}
			a.srv.Broadcast(protocol.VoiceStateEvent{State: state})
		}

	case protocol.TypeInterrupt:
		a.chor.Stop()
		a.speech.Stop() // 打断正在播放的语音
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})

	default:
		a.log.Debug("未处理的命令类型", "type", in.Env.Type)
	}
}

// handleChat 跑一次完整对话：用户消息回显 → 思考 → 流式生成 → 朗读(模拟)。
func (a *app) handleChat(ctx context.Context, text string) {
	if text == "" {
		return
	}

	// 1) 记录并回显用户消息。
	a.appendHistory(llm.Message{Role: llm.RoleUser, Content: text})
	a.srv.Broadcast(protocol.ChatEvent{
		ID:      a.nextMsgID(),
		Role:    protocol.RoleUser,
		Content: text,
		Status:  protocol.ChatFinal,
	})

	// 2) 进入思考态。
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceThinking})

	// 3) 流式生成助手回复。
	ch, err := a.llm.Chat(ctx, llm.Request{Messages: a.historySnapshot()})
	if err != nil {
		a.srv.Broadcast(protocol.ErrorEvent{Code: "llm_error", Message: err.Error()})
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
		return
	}

	id := a.nextMsgID()
	var content string
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceSpeaking})
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStart})

	for chunk := range ch {
		if chunk.Err != nil {
			a.srv.Broadcast(protocol.ErrorEvent{Code: "llm_stream", Message: chunk.Err.Error()})
			break
		}
		if chunk.Done {
			break
		}
		content += chunk.Delta
		a.srv.Broadcast(protocol.ChatEvent{
			ID:      id,
			Role:    protocol.RoleAssistant,
			Content: content,
			Status:  protocol.ChatStreaming,
		})
	}

	// 4) 收尾：最终消息 + 入历史。
	a.srv.Broadcast(protocol.ChatEvent{
		ID:      id,
		Role:    protocol.RoleAssistant,
		Content: content,
		Status:  protocol.ChatFinal,
	})
	if content != "" {
		a.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: content})
		// 交给语音服务朗读（Mock 仅记录；Sidecar 会真正合成播放）。
		if err := a.speech.Speak(ctx, content); err != nil {
			a.log.Warn("语音合成失败", "err", err)
		}
	}

	// 结束朗读，回到待命。
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStop, Text: content})
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
}

// handleJog 手动微调单个舵机：读回当前角度→改其一→下发→反馈。
func (a *app) handleJog(cmd protocol.JogJointCommand) {
	if cmd.Joint < 0 || cmd.Joint >= robot.JointCount {
		a.log.Warn("舵机序号越界", "joint", cmd.Joint)
		return
	}
	angles := a.bot.JointAngles()
	angles[cmd.Joint] = cmd.Angle
	if err := a.bot.SetJointAngles(angles, cmd.Enable); err != nil {
		a.log.Warn("设置舵机失败", "err", err)
		return
	}
	if err := a.bot.Sync(); err != nil {
		a.log.Warn("同步舵机失败", "err", err)
		return
	}
	a.srv.Broadcast(protocol.JointsEvent{Angles: a.bot.JointAngles(), Enabled: cmd.Enable})
}

// statusSnapshot 构造当前各子系统的状态快照。
func (a *app) statusSnapshot() protocol.StatusEvent {
	models := make([]protocol.ModelInfo, 0)
	for _, info := range a.llm.List() {
		models = append(models, protocol.ModelInfo{ID: info.ID, Name: info.Name, Provider: info.Provider})
	}
	ss := a.speech.Status()
	return protocol.StatusEvent{
		Robot: protocol.RobotStatus{
			Connected: a.bot.Connected(),
			VID:       0x1001, // ElectronBot 设备标识（Mock 下仅作展示）
			PID:       0x8023,
			FPS:       30,
		},
		ASR: protocol.ServiceStatus{Running: ss.ASRRunning, Detail: ss.Detail},
		TTS: protocol.ServiceStatus{Running: ss.TTSRunning, Detail: ss.Detail},
		LLM: protocol.LLMStatus{Active: a.llm.ActiveID(), Available: models},
	}
}

// ---- 对话历史的并发安全访问 ----

func (a *app) appendHistory(m llm.Message) {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	a.history = append(a.history, m)
	// 超出上限时裁剪，但始终保留首条系统提示。
	if len(a.history) > maxHistory+1 {
		a.history = append(a.history[:1], a.history[len(a.history)-maxHistory:]...)
	}
}

func (a *app) historySnapshot() []llm.Message {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	out := make([]llm.Message, len(a.history))
	copy(out, a.history)
	return out
}

func (a *app) nextMsgID() string {
	return fmt.Sprintf("m%d", a.msgSeq.Add(1))
}
