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
	"sort"
	"sync"
	"sync/atomic"

	"github.com/kingGang/ElectronStudio/internal/choreography"
	"github.com/kingGang/ElectronStudio/internal/config"
	"github.com/kingGang/ElectronStudio/internal/llm"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/robot"
	"github.com/kingGang/ElectronStudio/internal/robot/electronbot"
	"github.com/kingGang/ElectronStudio/internal/server"
	"github.com/kingGang/ElectronStudio/internal/speech"
	"github.com/kingGang/ElectronStudio/internal/tools"
	"github.com/kingGang/ElectronStudio/web"
)

// systemPrompt 是对话的系统提示，约束助手扮演桌面机器人小电。
const systemPrompt = "你是桌面机器人小电，回答简洁友好。"

// maxHistory 限制保留的对话轮数，防止上下文无限增长。
const maxHistory = 20

func main() {
	addr := flag.String("addr", "", "HTTP/WebSocket 监听地址（覆盖配置文件）")
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.Addr = *addr // 命令行优先
	}

	a, err := newApp(cfg, *cfgPath, log)
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

	if err := a.srv.Serve(ctx, cfg.Addr); err != nil {
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
	tools  *tools.Registry
	log    *slog.Logger

	cfgMu   sync.Mutex     // 保护 cfg 的读改存
	cfg     *config.Config // 当前配置
	cfgPath string         // 配置文件路径

	histMu  sync.Mutex
	history []llm.Message  // 对话历史（含系统提示）
	msgSeq  atomic.Uint64  // 对话消息 ID 自增源
}

// newApp 组装所有依赖。
func newApp(cfg *config.Config, cfgPath string, log *slog.Logger) (*app, error) {
	// 机器人：按配置选择传输（auto 优先尝试真机 USB，失败回退 Mock）。
	bot := connectRobot(cfg, log)

	// 动作编排：注册内置动作。
	chor := choreography.NewEngine(bot, choreography.WithLogger(log))
	for _, act := range choreography.DefaultActions() {
		chor.Register(act)
	}

	// 大模型：从配置构建路由（至少含 Echo 兜底）。
	router := llm.NewRouter()
	for _, mc := range cfg.Models {
		router.Add(providerFromConfig(mc))
	}
	if cfg.Active != "" {
		_ = router.SetActive(cfg.Active)
	}

	// 语音：默认 Mock（无 sidecar 也能跑文本链路）；sidecar 地址优先取配置，其次环境变量。
	sidecarURL := cfg.Speech.SidecarURL
	if sidecarURL == "" {
		sidecarURL = os.Getenv("SPEECH_SIDECAR_URL")
	}
	var voice speech.Service
	if sidecarURL != "" {
		voice = speech.NewSidecar(sidecarURL, log)
		log.Info("已配置语音 sidecar", "url", sidecarURL)
	} else {
		voice = speech.NewMock(log)
	}

	a := &app{
		chor:    chor,
		llm:     router,
		bot:     bot,
		speech:  voice,
		log:     log,
		cfg:     cfg,
		cfgPath: cfgPath,
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

	// 工具：注册可供大模型调用的工具（设备控制 / 情绪 / 动作 / 信息）。
	a.tools = buildTools(a)
	return a, nil
}

// connectRobot 依据配置选择并连接机器人传输：
//   - "mock"：始终使用 Mock；
//   - "electronbot"：连真机，失败则告警并回退 Mock；
//   - "auto"（默认）：尝试真机，未检测到则静默回退 Mock。
//
// 这样真机插上即用，无真机时开发/演示照常进行。
func connectRobot(cfg *config.Config, log *slog.Logger) robot.Transport {
	mode := cfg.Robot
	if mode == "" {
		mode = "auto"
	}

	if mode != "mock" {
		dev := electronbot.New(log)
		if err := dev.Connect(); err == nil {
			return dev
		} else if mode == "electronbot" {
			log.Warn("指定 electronbot 但连接失败，回退 Mock", "err", err)
		} else {
			log.Info("未检测到 ElectronBot，使用 Mock", "reason", err)
		}
	}

	m := robot.NewMock(log)
	_ = m.Connect()
	return m
}

// buildTools 构造工具注册表，副作用以闭包注入（情绪/动作走机器人，台灯为内置设备）。
func buildTools(a *app) *tools.Registry {
	reg := tools.NewRegistry()

	emotions := []string{"neutral", "happy", "sad", "angry", "surprised", "confused"}
	reg.Register(tools.EmotionTool(emotions, func(e string) error {
		a.srv.Broadcast(protocol.EmotionEvent{Emotion: protocol.Emotion(e)})
		if _, ok := a.chor.Lookup(e); ok {
			_ = a.chor.Play(context.Background(), e, 1)
		}
		return nil
	}))

	actionNames := a.chor.Names()
	sort.Strings(actionNames)
	reg.Register(tools.ActionTool(actionNames, func(name string) error {
		return a.chor.Play(context.Background(), name, 1)
	}))

	reg.Register(tools.TimeTool())
	reg.Register(tools.NewLamp().Tool())
	return reg
}

// providerFromConfig 依据配置条目构造一个 LLM Provider。
func providerFromConfig(mc config.ModelConfig) llm.Provider {
	id := mc.ID
	if id == "" {
		id = mc.Type + ":" + mc.Model
	}
	switch mc.Type {
	case "openai":
		return llm.NewOpenAICompat(llm.OpenAIConfig{
			ID: id, Name: mc.Name, BaseURL: mc.BaseURL, APIKey: mc.APIKey, Model: mc.Model,
		})
	default: // echo 及未知类型一律回退为本地回声，避免启动失败
		name := mc.Name
		if name == "" {
			name = "本地回声"
		}
		return llm.NewEcho(id, name)
	}
}

// saveConfig 持久化当前配置（已加锁的调用方使用）。
func (a *app) saveConfig() {
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.log.Warn("保存配置失败", "err", err)
	}
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
			} else {
				a.cfgMu.Lock()
				a.cfg.SetActive(cmd.ID)
				a.saveConfig()
				a.cfgMu.Unlock()
			}
			a.srv.Broadcast(a.statusSnapshot()) // 让所有端同步当前模型
		}

	case protocol.TypeAddModel:
		if cmd, err := protocol.As[protocol.AddModelCommand](in.Env); err == nil {
			a.handleAddModel(cmd)
		}

	case protocol.TypeRemoveModel:
		if cmd, err := protocol.As[protocol.RemoveModelCommand](in.Env); err == nil {
			a.handleRemoveModel(cmd)
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

// handleChat 是对话入口：当生效模型支持工具调用且已注册工具时走工具循环，
// 否则走流式对话。
func (a *app) handleChat(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if a.tools.Count() > 0 && a.llm.ActiveSupportsTools() {
		a.handleChatWithTools(ctx, text)
	} else {
		a.handleChatStreaming(ctx, text)
	}
}

// emitUser 记录并回显一条用户消息，随后进入思考态。
func (a *app) emitUser(text string) {
	a.appendHistory(llm.Message{Role: llm.RoleUser, Content: text})
	a.srv.Broadcast(protocol.ChatEvent{
		ID:      a.nextMsgID(),
		Role:    protocol.RoleUser,
		Content: text,
		Status:  protocol.ChatFinal,
	})
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceThinking})
}

// finishAssistant 完成一条助手回复：入历史、朗读、回到待命。
func (a *app) finishAssistant(ctx context.Context, content string) {
	if content != "" {
		a.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: content})
		if err := a.speech.Speak(ctx, content); err != nil {
			a.log.Warn("语音合成失败", "err", err)
		}
	}
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStop, Text: content})
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
}

// handleChatWithTools 走 function-calling 工具循环：模型可调用设备/动作/信息类工具。
func (a *app) handleChatWithTools(ctx context.Context, text string) {
	a.emitUser(text)

	// tools.Spec → llm.Tool。
	specs := a.tools.Specs()
	lt := make([]llm.Tool, 0, len(specs))
	for _, s := range specs {
		lt = append(lt, llm.Tool{Name: s.Name, Description: s.Description, Parameters: s.Parameters})
	}

	id := a.nextMsgID()
	var pTools []protocol.ToolCall
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceSpeaking})
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStart})

	res, err := llm.RunToolLoop(ctx, a.llm, a.historySnapshot(), lt, a.tools.Execute, 5,
		func(ec llm.ExecutedCall) {
			// 每执行一个工具，实时把工具徽章推给前端。
			status := "ok"
			if ec.Err != "" {
				status = "error"
			}
			pTools = append(pTools, protocol.ToolCall{
				ID: ec.ID, Name: ec.Name, Arguments: ec.Arguments, Result: ec.Result, Status: status,
			})
			a.srv.Broadcast(protocol.ChatEvent{
				ID: id, Role: protocol.RoleAssistant, Tools: pTools, Status: protocol.ChatStreaming,
			})
		})
	if err != nil {
		a.srv.Broadcast(protocol.ErrorEvent{Code: "llm_error", Message: err.Error()})
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
		return
	}

	a.srv.Broadcast(protocol.ChatEvent{
		ID: id, Role: protocol.RoleAssistant, Content: res.Content, Tools: pTools, Status: protocol.ChatFinal,
	})
	a.finishAssistant(ctx, res.Content)
}

// handleChatStreaming 跑一次流式对话：用户消息回显 → 思考 → 流式生成 → 朗读。
func (a *app) handleChatStreaming(ctx context.Context, text string) {
	// 1) 记录并回显用户消息 + 进入思考态。
	a.emitUser(text)

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

	// 收尾：最终消息 + 入历史 + 朗读 + 回到待命。
	a.srv.Broadcast(protocol.ChatEvent{
		ID:      id,
		Role:    protocol.RoleAssistant,
		Content: content,
		Status:  protocol.ChatFinal,
	})
	a.finishAssistant(ctx, content)
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

// handleAddModel 新增/编辑一个模型：更新路由、持久化配置、广播状态。
func (a *app) handleAddModel(cmd protocol.AddModelCommand) {
	mc := config.ModelConfig{
		ID: cmd.ID, Name: cmd.Name, Type: cmd.Kind,
		BaseURL: cmd.BaseURL, APIKey: cmd.APIKey, Model: cmd.Model,
	}
	a.cfgMu.Lock()
	id := a.cfg.Upsert(mc)
	mc.ID = id
	a.saveConfig()
	a.cfgMu.Unlock()

	// 重建该 Provider 并加入路由（Add 同 ID 会覆盖）。
	a.llm.Add(providerFromConfig(mc))
	a.log.Info("已添加/更新模型", "id", id, "type", mc.Type)
	a.srv.Broadcast(a.statusSnapshot())
}

// handleRemoveModel 删除一个模型：从路由与配置移除并持久化。
func (a *app) handleRemoveModel(cmd protocol.RemoveModelCommand) {
	a.cfgMu.Lock()
	removed := a.cfg.Remove(cmd.ID)
	active := a.cfg.Active
	a.saveConfig()
	a.cfgMu.Unlock()
	if !removed {
		return
	}
	a.llm.Remove(cmd.ID)
	_ = a.llm.SetActive(active) // 与配置归一化后的 active 对齐
	a.log.Info("已删除模型", "id", cmd.ID)
	a.srv.Broadcast(a.statusSnapshot())
}

// statusSnapshot 构造当前各子系统的状态快照。
func (a *app) statusSnapshot() protocol.StatusEvent {
	models := make([]protocol.ModelInfo, 0)
	for _, info := range a.llm.List() {
		models = append(models, protocol.ModelInfo{ID: info.ID, Name: info.Name, Provider: info.Provider})
	}
	ss := a.speech.Status()
	actions := a.chor.Names()
	sort.Strings(actions) // 稳定排序，避免前端按钮顺序抖动
	return protocol.StatusEvent{
		Robot: protocol.RobotStatus{
			Connected: a.bot.Connected(),
			VID:       0x1001, // ElectronBot 设备标识（Mock 下仅作展示）
			PID:       0x8023,
			FPS:       30,
		},
		ASR:     protocol.ServiceStatus{Running: ss.ASRRunning, Detail: ss.Detail},
		TTS:     protocol.ServiceStatus{Running: ss.TTSRunning, Detail: ss.Detail},
		LLM:     protocol.LLMStatus{Active: a.llm.ActiveID(), Available: models},
		Actions: actions,
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
