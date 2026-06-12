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
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kingGang/ElectronStudio/internal/choreography"
	"github.com/kingGang/ElectronStudio/internal/config"
	"github.com/kingGang/ElectronStudio/internal/device"
	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/gesture"
	"github.com/kingGang/ElectronStudio/internal/llm"
	"github.com/kingGang/ElectronStudio/internal/music"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/robot"
	"github.com/kingGang/ElectronStudio/internal/robot/electronbot"
	"github.com/kingGang/ElectronStudio/internal/scheduler"
	"github.com/kingGang/ElectronStudio/internal/server"
	"github.com/kingGang/ElectronStudio/internal/speech"
	"github.com/kingGang/ElectronStudio/internal/tools"
	"github.com/kingGang/ElectronStudio/internal/weather"
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
	defer a.music.Close()

	// 启动手势服务（Mock 为空操作；Sidecar 连接外部识别进程）。
	if err := a.gesture.Start(ctx); err != nil {
		log.Warn("手势服务启动失败", "err", err)
	}
	defer a.gesture.Close()

	// 启动定时任务调度。
	go a.sched.Run(ctx)

	// 启动设备驱动循环（统一推帧 + 同步）。
	go a.driver.Run(ctx)

	// 消费入站命令、上行语音事件与手势事件。
	go a.dispatchLoop(ctx)
	go a.speechLoop(ctx)
	go a.gestureLoop(ctx)

	if err := a.srv.Serve(ctx, cfg.Addr); err != nil {
		log.Error("服务退出", "err", err)
		os.Exit(1)
	}
}

// app 持有各模块依赖与少量运行时状态，并实现命令分发。
type app struct {
	srv     *server.Server
	chor    *choreography.Engine
	llm     *llm.Router
	bot     robot.Transport
	driver  *device.Driver        // 统一设备驱动（拥有 Sync）
	screen  *display.Compositor   // 屏幕画面合成（摄像头 / 素材片 / 程序动画脸）
	clips   *display.ClipSource   // 离线表情素材片（支持界面上传/删除后热重载）
	camera  *display.CameraSource // 摄像头采集（可为 nil）

	emotionsDir string     // 表情素材目录（与 config.json 同目录的 emotions/）
	matMu       sync.Mutex // 串行化素材上传/删除，避免并发改写同一情绪文件
	speech  speech.Service
	gesture gesture.Service
	music   *music.Service
	weather *weather.Client
	sched   *scheduler.Scheduler
	schedPath string
	tools   *tools.Registry
	log     *slog.Logger

	frameSeq atomic.Uint32 // 屏幕镜像帧序号

	cfgMu   sync.Mutex     // 保护 cfg 的读改存
	cfg     *config.Config // 当前配置
	cfgPath string         // 配置文件路径

	actionsPath string        // 录制动作的存盘路径
	poseMu      sync.Mutex    // 保护 desiredPose
	desiredPose robot.Joints  // 手动/跟随退出时保持的目标姿态

	recMu     sync.Mutex             // 保护录制状态
	recording bool                   // 是否正在录制
	recName   string                 // 当前录制动作名
	recFrames []choreography.Keyframe // 已采集的关键帧
	recStart  time.Time              // 录制起点

	histMu  sync.Mutex
	history []llm.Message  // 对话历史（含系统提示）
	msgSeq  atomic.Uint64  // 对话消息 ID 自增源
}

// newApp 组装所有依赖。
func newApp(cfg *config.Config, cfgPath string, log *slog.Logger) (*app, error) {
	// 机器人：按配置选择传输（auto 优先尝试真机 USB，失败回退 Mock）。
	bot := connectRobot(cfg, log)
	actionsPath := filepath.Join(filepath.Dir(cfgPath), "actions.json")

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

	// 手势：默认 Mock；配置了 sidecar 地址则对接真实手势识别（MediaPipe 等）。
	var ges gesture.Service
	if url := cfg.Gesture.SidecarURL; url != "" {
		ges = gesture.NewSidecar(url, log)
		log.Info("已配置手势 sidecar", "url", url)
	} else {
		ges = gesture.NewMock(log)
	}

	a := &app{
		llm:         router,
		bot:         bot,
		speech:      voice,
		gesture:     ges,
		log:         log,
		cfg:         cfg,
		cfgPath:     cfgPath,
		actionsPath: actionsPath,
		history:     []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}},
	}

	// 新客户端连上时，推送一次状态快照，让前端立即渲染。
	a.srv = server.New(server.Options{
		Logger:   log,
		StaticFS: web.FS(),        // 内嵌前端单页应用，访问 http://addr/ 即是界面
		Routes:   a.materialRoutes, // 素材管理 REST 接口（上传/缩略图）
		OnConnect: func(c *server.Client) {
			if err := c.Send(a.statusSnapshot()); err != nil {
				log.Warn("推送初始状态失败", "err", err)
			}
			_ = c.Send(a.scheduleListEvent()) // 推送当前定时任务列表
			_ = c.Send(a.materialsEvent())    // 推送当前屏幕表情素材列表
		},
	})

	// 画面源：摄像头(可选) + 离线素材片 + 程序实时动画脸(眨眼/口型, 兜底)。
	// 统一设备驱动以固定帧率把"姿态 + 画面"一并 Sync 给设备，并把同一帧广播给 UI，实现镜像同步。
	face := display.NewEmotionSource()
	a.emotionsDir = filepath.Join(filepath.Dir(cfgPath), "emotions")
	clips, err := display.LoadClips(a.emotionsDir)
	if err != nil {
		log.Warn("加载表情素材失败，使用程序动画脸", "err", err)
	}
	if len(clips) > 0 {
		log.Info("已加载表情素材", "情绪数", len(clips))
	}
	if cfg.Camera.Enabled {
		a.camera = display.NewCameraSource(log)
	}
	a.clips = display.NewClipSource(clips)
	a.screen = display.NewCompositor(a.camera, a.clips, face)
	a.driver = device.NewDriver(bot, a.screen, log, 30, a.onDriverFrame, a.onDriverJoints)

	// 动作编排：把姿态写入驱动；注册内置动作并加载用户录制的动作（同名覆盖内置）。
	a.chor = choreography.NewEngine(a.driver, choreography.WithLogger(log))
	for _, act := range choreography.DefaultActions() {
		a.chor.Register(act)
	}
	if err := a.chor.LoadActions(actionsPath); err != nil {
		log.Warn("加载录制动作失败", "err", err)
	}

	// 音乐：酷我搜索 + mpg123 播放（子进程，无 cgo），状态变化广播给 UI。
	a.music = music.NewService(
		music.NewKuwoSearcher(),
		music.NewMpg123Player(cfg.Music.Mpg123, log),
		log,
		func(t music.Track, st music.State) {
			a.srv.Broadcast(protocol.MusicEvent{State: string(st), Name: t.Name, Artist: t.Artist})
		},
	)

	// 天气 + 定时任务（提醒/闹钟/周期）。
	a.weather = weather.New()
	a.schedPath = filepath.Join(filepath.Dir(cfgPath), "jobs.json")
	a.sched = scheduler.New(a.onJobFire, log)
	if err := a.sched.Load(a.schedPath); err != nil {
		log.Warn("加载定时任务失败", "err", err)
	}
	a.sched.SetPath(a.schedPath) // Load 后再设路径，触发/增删后自动存盘

	// 工具：注册可供大模型调用的工具（设备控制 / 情绪 / 动作 / 信息 / 音乐 / 天气 / 提醒）。
	a.tools = buildTools(a)
	return a, nil
}

// onJobFire 在定时任务到点时执行其动作。
func (a *app) onJobFire(j scheduler.Job) {
	ctx := context.Background()
	switch j.Action.Kind {
	case "weather":
		if txt, err := a.weather.Get(ctx, j.Action.Query); err == nil {
			a.say(ctx, txt)
		}
	case "greet":
		a.handleGreet(ctx)
	case "music":
		_, _ = a.music.SearchAndPlay(ctx, j.Action.Query)
	default: // say
		if j.Action.Text != "" {
			a.say(ctx, j.Action.Text)
		}
	}
}

// say 让机器人主动说一句（广播为助手消息 + 语音播报）。
func (a *app) say(ctx context.Context, text string) {
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceSpeaking})
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStart})
	a.screen.SetSpeaking(true)
	a.srv.Broadcast(protocol.ChatEvent{
		ID: a.nextMsgID(), Role: protocol.RoleAssistant, Content: text, Status: protocol.ChatFinal,
	})
	a.finishAssistant(ctx, text)
}

// scheduleListEvent 构造当前任务列表事件。
func (a *app) scheduleListEvent() protocol.ScheduleListEvent {
	jobs := a.sched.List()
	out := make([]protocol.ScheduleJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, protocol.ScheduleJob{
			ID: j.ID, Title: j.Title, At: j.At, Every: j.Every, Daily: j.Daily,
			Kind: j.Action.Kind, Text: j.Action.Text,
		})
	}
	return protocol.ScheduleListEvent{Jobs: out}
}

// onDriverFrame 把设备屏当前帧编码为二进制镜像帧广播给 UI（与设备屏同源 = 同步）。
func (a *app) onDriverFrame(rgb []byte) {
	hdr := protocol.FrameHeader{
		Width: robot.ScreenWidth, Height: robot.ScreenHeight,
		Format: protocol.PixelRGB888, Seq: a.frameSeq.Add(1),
	}
	if frame, err := protocol.EncodeFrame(hdr, rgb); err == nil {
		a.srv.BroadcastFrame(frame)
	}
}

// onDriverJoints 周期广播舵机真实角度（驱动统一上报）。
func (a *app) onDriverJoints(j robot.Joints) {
	a.srv.Broadcast(protocol.JointsEvent{Angles: j, Enabled: true})
}

// setEmotion 切换情绪：更新屏幕画面源、广播情绪、并播放同名动作（若有）。
func (a *app) setEmotion(e string) {
	a.screen.SetEmotion(e)
	a.srv.Broadcast(protocol.EmotionEvent{Emotion: protocol.Emotion(e)})
	if _, ok := a.chor.Lookup(e); ok {
		_ = a.chor.Play(context.Background(), e, 1)
	}
}

// previewEmotion 仅切换屏幕表情画面并广播情绪，不联动播放同名动作。
// 供素材管理页「预览」使用：预览一个与某动作同名的素材时，真机不应做出物理动作。
func (a *app) previewEmotion(e string) {
	a.screen.SetEmotion(e)
	a.srv.Broadcast(protocol.EmotionEvent{Emotion: protocol.Emotion(e)})
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
		a.setEmotion(e)
		return nil
	}))

	actionNames := a.chor.Names()
	sort.Strings(actionNames)
	reg.Register(tools.ActionTool(actionNames, func(name string) error {
		return a.chor.Play(context.Background(), name, 1)
	}))

	reg.Register(tools.TimeTool())
	reg.Register(tools.NewLamp().Tool())

	// 天气：查询某城市天气。
	reg.Register(tools.WeatherTool(func(ctx context.Context, city string) (string, error) {
		return a.weather.Get(ctx, city)
	}))

	// 提醒：N 分钟后提醒（或指定时间），到点机器人会说出来。
	reg.Register(tools.ReminderTool(func(_ context.Context, minutes int, text string) (string, error) {
		at := time.Now().Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
		a.handleScheduleAdd(protocol.ScheduleAddCommand{Title: text, At: at, Kind: "say", Text: text})
		return fmt.Sprintf("好的，%d 分钟后提醒你：%s", minutes, text), nil
	}))

	// 音乐：搜索并播放（大模型可"放首歌"）。
	reg.Register(tools.MusicTool(func(ctx context.Context, query string) (string, error) {
		t, err := a.music.SearchAndPlay(ctx, query)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("正在播放：%s - %s", t.Name, t.Artist), nil
	}))

	// 视觉："看一眼"——抓摄像头帧交给视觉模型描述（仅在配置了摄像头时提供）。
	if a.camera != nil {
		reg.Register(tools.LookTool(a.captureJPEG, func(ctx context.Context, jpeg []byte, q string) (string, error) {
			return a.llm.Vision(ctx, jpeg, q)
		}))
	}
	return reg
}

// captureJPEG 抓取当前摄像头画面并编码为 JPEG（必要时临时启动采集）。
func (a *app) captureJPEG(ctx context.Context) ([]byte, error) {
	if a.camera == nil {
		return nil, fmt.Errorf("摄像头未启用")
	}
	if !a.camera.Running() {
		cfg := a.cfg.Camera
		if err := a.camera.Start(ctx, display.CameraConfig{
			FFmpeg: cfg.FFmpeg, InputFormat: cfg.InputFormat, Input: cfg.Input,
		}); err != nil {
			return nil, err
		}
	}
	// 等待首帧（采集启动后约需 ~200ms）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f := a.camera.Snapshot(); f != nil {
			return display.EncodeJPEG(f)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("未取到摄像头画面")
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
			if cmd.Preview {
				a.previewEmotion(string(cmd.Emotion)) // 仅切屏 + 广播，不联动动作（素材预览）
			} else {
				a.setEmotion(string(cmd.Emotion)) // 更新屏幕画面 + 广播 + 同名动作
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

	case protocol.TypeFollow:
		if cmd, err := protocol.As[protocol.FollowCommand](in.Env); err == nil {
			a.handleFollow(ctx, cmd.Enable)
		}

	case protocol.TypeRecordStart:
		if cmd, err := protocol.As[protocol.RecordStartCommand](in.Env); err == nil {
			a.recordStart(cmd.Name)
		}

	case protocol.TypeRecordFrame:
		a.recordFrame()

	case protocol.TypeRecordStop:
		a.recordStop()

	case protocol.TypeDeleteAction:
		if cmd, err := protocol.As[protocol.DeleteActionCommand](in.Env); err == nil {
			a.deleteAction(cmd.Name)
		}

	case protocol.TypeCamera:
		if cmd, err := protocol.As[protocol.CameraCommand](in.Env); err == nil {
			a.handleCamera(ctx, cmd.Enable)
		}

	case protocol.TypeGreet:
		go a.handleGreet(ctx) // 看一眼打招呼，放后台

	case protocol.TypeMusic:
		if cmd, err := protocol.As[protocol.MusicCommand](in.Env); err == nil {
			go a.handleMusic(ctx, cmd)
		}

	case protocol.TypeScheduleAdd:
		if cmd, err := protocol.As[protocol.ScheduleAddCommand](in.Env); err == nil {
			a.handleScheduleAdd(cmd)
		}

	case protocol.TypeScheduleRemove:
		if cmd, err := protocol.As[protocol.ScheduleRemoveCommand](in.Env); err == nil {
			a.sched.Remove(cmd.ID)
			a.srv.Broadcast(a.scheduleListEvent())
		}

	case protocol.TypeMaterialDelete:
		if cmd, err := protocol.As[protocol.MaterialDeleteCommand](in.Env); err == nil {
			go a.handleMaterialDelete(cmd.Name) // 涉及磁盘+重载，放后台，避免阻塞命令分发循环
		}

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
	a.screen.SetSpeaking(false) // 停止口型动画
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
	a.screen.SetSpeaking(true) // 驱动口型动画

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
	a.screen.SetSpeaking(true) // 驱动口型动画

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

// handleJog 手动微调单个舵机：更新目标姿态并交给驱动下发（角度反馈由驱动统一广播）。
func (a *app) handleJog(cmd protocol.JogJointCommand) {
	if cmd.Joint < 0 || cmd.Joint >= robot.JointCount {
		a.log.Warn("舵机序号越界", "joint", cmd.Joint)
		return
	}
	a.poseMu.Lock()
	a.desiredPose[cmd.Joint] = cmd.Angle
	pose := a.desiredPose
	a.poseMu.Unlock()
	a.driver.SetPose(pose, cmd.Enable)
}

// handleFollow 开关"跟随设备"（实体优先）：开启时让舵机松力（enable=false），
// 用户掰动机器人，驱动每帧读回真实角度并广播，界面滑块随之跟随；关闭时保持当前姿态。
func (a *app) handleFollow(_ context.Context, enable bool) {
	if enable {
		a.driver.SetPose(robot.Joints{}, false) // 松力，可手动摆姿
		a.log.Info("跟随设备已开启")
		return
	}
	a.poseMu.Lock()
	pose := a.desiredPose
	a.poseMu.Unlock()
	a.driver.SetPose(pose, true) // 保持当前目标姿态
	a.log.Info("跟随设备已关闭")
}

// recordStart 开始录制一段动作。
func (a *app) recordStart(name string) {
	a.recMu.Lock()
	a.recording = true
	a.recName = name
	a.recFrames = nil
	a.recStart = time.Now()
	a.recMu.Unlock()
	a.log.Info("开始录制动作", "name", name)
}

// recordFrame 把当前姿态采集为一个关键帧（时间戳相对录制起点）。
func (a *app) recordFrame() {
	a.recMu.Lock()
	defer a.recMu.Unlock()
	if !a.recording {
		return
	}
	at := int(time.Since(a.recStart) / time.Millisecond)
	a.recFrames = append(a.recFrames, choreography.Keyframe{AtMs: at, Angles: a.bot.JointAngles()})
	a.log.Debug("采集关键帧", "序号", len(a.recFrames), "at_ms", at)
}

// recordStop 结束录制：注册为动作、存盘、广播更新后的动作列表。
func (a *app) recordStop() {
	a.recMu.Lock()
	if !a.recording {
		a.recMu.Unlock()
		return
	}
	a.recording = false
	name, frames := a.recName, a.recFrames
	a.recMu.Unlock()

	if name == "" || len(frames) == 0 {
		a.log.Warn("录制为空，已丢弃", "name", name, "frames", len(frames))
		a.srv.Broadcast(a.statusSnapshot())
		return
	}
	a.chor.Register(choreography.Action{Name: name, Loops: 1, Frames: frames})
	if err := a.chor.SaveActions(a.actionsPath); err != nil {
		a.log.Warn("保存动作失败", "err", err)
	}
	a.log.Info("动作已录制保存", "name", name, "frames", len(frames))
	a.srv.Broadcast(a.statusSnapshot())
}

// deleteAction 删除一段动作并存盘。
func (a *app) deleteAction(name string) {
	a.chor.Unregister(name)
	if err := a.chor.SaveActions(a.actionsPath); err != nil {
		a.log.Warn("保存动作失败", "err", err)
	}
	a.srv.Broadcast(a.statusSnapshot())
}

// gestureLoop 消费手势事件并映射为机器人行为。
func (a *app) gestureLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-a.gesture.Events():
			if !ok {
				return
			}
			a.handleGesture(ctx, ev)
		}
	}
}

// handleGesture 把一个手势映射为行为，并广播给 UI。
func (a *app) handleGesture(ctx context.Context, ev gesture.Event) {
	a.log.Info("识别到手势", "name", ev.Name, "conf", ev.Confidence)
	a.srv.Broadcast(protocol.GestureEvent{Name: ev.Name, Confidence: ev.Confidence})

	switch ev.Name {
	case "wave": // 挥手 → 看一眼打招呼
		a.handleGreet(ctx)
	case "thumbs_up": // 点赞 → 开心 + 点头
		a.setEmotion("happy")
		_ = a.chor.Play(ctx, "nod", 1)
	case "victory": // 比耶 → 庆祝
		a.setEmotion("happy")
		_ = a.chor.Play(ctx, "cheer", 1)
	case "open_palm", "stop": // 张开手掌 → 停止当前动作/语音
		a.chor.Stop()
		a.speech.Stop()
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
	case "fist": // 握拳 → 摇头
		_ = a.chor.Play(ctx, "shake", 1)
	default:
		a.log.Debug("未映射的手势", "name", ev.Name)
	}
}

// handleScheduleAdd 新增一个定时任务并存盘、广播。
func (a *app) handleScheduleAdd(cmd protocol.ScheduleAddCommand) {
	kind := cmd.Kind
	if kind == "" {
		kind = "say"
	}
	job := scheduler.Job{
		Title: cmd.Title, At: cmd.At, Every: cmd.Every, Daily: cmd.Daily,
		Action: scheduler.Action{Kind: kind, Text: cmd.Text, Query: cmd.Query},
	}
	if _, err := a.sched.Add(job); err != nil {
		a.log.Warn("添加定时任务失败", "err", err)
		a.srv.Broadcast(protocol.ErrorEvent{Code: "schedule_error", Message: err.Error()})
		return
	}
	a.srv.Broadcast(a.scheduleListEvent())
}

// handleMusic 处理音乐控制命令。
func (a *app) handleMusic(ctx context.Context, cmd protocol.MusicCommand) {
	switch cmd.Action {
	case "play":
		if _, err := a.music.SearchAndPlay(ctx, cmd.Query); err != nil {
			a.log.Warn("播放音乐失败", "query", cmd.Query, "err", err)
			a.srv.Broadcast(protocol.ErrorEvent{Code: "music_error", Message: err.Error()})
		}
	case "pause":
		a.music.Pause()
	case "resume":
		a.music.Resume()
	case "stop":
		a.music.Stop()
	case "volume":
		a.music.SetVolume(cmd.Volume)
	default:
		a.log.Debug("未知音乐动作", "action", cmd.Action)
	}
}

// handleGreet 看一眼打招呼：笑脸 + 挥手 + （有摄像头则看一眼）+ 说一句招呼。
func (a *app) handleGreet(ctx context.Context) {
	a.setEmotion("happy")
	_ = a.chor.Play(ctx, "wave", 1)

	text := a.greetingText(ctx)

	id := a.nextMsgID()
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceSpeaking})
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStart})
	a.screen.SetSpeaking(true)
	a.srv.Broadcast(protocol.ChatEvent{
		ID: id, Role: protocol.RoleAssistant, Content: text, Status: protocol.ChatFinal,
	})
	a.finishAssistant(ctx, text)
}

// greetingText 生成招呼语：有摄像头则让视觉模型"看一眼"后打招呼，否则用友好问候兜底。
func (a *app) greetingText(ctx context.Context) string {
	if a.camera != nil {
		if jpeg, err := a.captureJPEG(ctx); err == nil {
			if t, err := a.llm.Vision(ctx, jpeg,
				"你是桌面机器人小电。看一眼面前的画面，用一句友好、简短的中文主动跟对方打招呼（不超过20字）。"); err == nil && t != "" {
				return t
			}
		}
	}
	greetings := []string{"你好呀，我是小电！", "嗨，见到你真高兴！", "你好，今天过得怎么样？"}
	return greetings[int(time.Now().UnixNano())%len(greetings)]
}

// handleCamera 开关屏幕显示摄像头画面：开启时启动 ffmpeg 采集并切到摄像头，关闭时停采并切回表情脸。
func (a *app) handleCamera(ctx context.Context, enable bool) {
	if a.camera == nil {
		a.log.Warn("未启用摄像头（config.camera.enabled=false）")
		return
	}
	if enable {
		cfg := a.cfg.Camera
		if err := a.camera.Start(ctx, display.CameraConfig{
			FFmpeg: cfg.FFmpeg, InputFormat: cfg.InputFormat, Input: cfg.Input,
		}); err != nil {
			a.log.Warn("启动摄像头失败", "err", err)
			a.srv.Broadcast(protocol.ErrorEvent{Code: "camera_error", Message: err.Error()})
			return
		}
		a.screen.SetCamera(true)
	} else {
		a.screen.SetCamera(false)
		a.camera.Stop()
	}
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
		Camera:  a.camera != nil,
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
