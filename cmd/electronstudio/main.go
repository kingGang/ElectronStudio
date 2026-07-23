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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kingGang/ElectronStudio/internal/audioout"
	"github.com/kingGang/ElectronStudio/internal/choreography"
	"github.com/kingGang/ElectronStudio/internal/config"
	"github.com/kingGang/ElectronStudio/internal/device"
	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/gesture"
	"github.com/kingGang/ElectronStudio/internal/llm"
	"github.com/kingGang/ElectronStudio/internal/minimax"
	"github.com/kingGang/ElectronStudio/internal/music"
	"github.com/kingGang/ElectronStudio/internal/netspeech"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/realtime"
	"github.com/kingGang/ElectronStudio/internal/robot"
	"github.com/kingGang/ElectronStudio/internal/robot/electronbot"
	"github.com/kingGang/ElectronStudio/internal/scheduler"
	"github.com/kingGang/ElectronStudio/internal/server"
	"github.com/kingGang/ElectronStudio/internal/speech"
	"github.com/kingGang/ElectronStudio/internal/tools"
	"github.com/kingGang/ElectronStudio/internal/weather"
	"github.com/kingGang/ElectronStudio/internal/xiaozhi"
	"github.com/kingGang/ElectronStudio/web"
)

// defaultPersona 是默认的设备角色/人设；可被 config.persona 覆盖（设置页可改）。
const defaultPersona = "你是桌面机器人小电，回答简洁友好。"

// toolRules 是固定的工具使用铁律，始终拼在人设之后——确保换了人设也不破坏"动作必须调工具"。
// 关键：MiniMax-M3 等模型对模糊指令容易只口头回应而不实际调用工具，这里明确禁止。
const toolRules = `

你能调用工具完成实际动作：播放音乐(play_music)、音乐控制(music_control)、生成音乐(generate_music)、生成图片(generate_image)、设提醒/定时(schedule)、看摄像头(look)、【换表情情绪(set_emotion)】、播放动作等。

你有一张会发光的脸，可用 set_emotion 切换表情，情绪可选：neutral(平静)、happy(开心)、sad(难过)、angry(生气)、surprised(惊讶)、confused(疑惑)、silly(鬼脸/搞怪)。

铁律：
1. 凡是用户要你"做一件事"（放歌、放点音乐、来首歌、换歌、暂停、生成音乐/图片、设提醒等），必须实际调用对应工具，绝不能只用文字说"好的我帮你放/已为你换"却不调用工具。
1b. 用户要你【做表情】（如"生气/开心/难过一下""做个鬼脸""卖个萌""摆个惊讶脸"），直接调 set_emotion 换上对应情绪，并用一句话应和（如"哼，生气啦！"）。你【绝对不能说自己不会做表情、没有这个功能】——你就是有一张会表情的脸。
2. 指令模糊时（如"随便放首歌""放点音乐""来点轻松的"），你自己定一个具体歌名或歌手直接调 play_music，不要反问用户"想听什么"。
3. 音乐工具怎么选：
   - "放一首X/放歌/听歌/换个歌手"→ play_music（带歌名或歌手）
   - "换一首/下一首/切歌/换个"且没指定具体歌名 → music_control，action=next（切到当前列表下一首，不要重新搜索）
   - "上一首"→ music_control prev；"暂停"→ pause；"继续"→ resume；"停/别放了"→ stop
   - "生成/创作一段音乐"→ generate_music
4. 工具执行后再用一句话简短告知结果。
5. 你是【语音助手】，回答会被朗读出来，所以必须口语化、简短：一两句话讲清重点，像人当面说话那样直接。禁止 markdown、标题、列表符号、表情符号、代码块、网址；不要"首先/其次"罗列，不要"好的，我来帮你"之类客套铺垫，直接说答案。`

// buildSystemPrompt 组合"人设 + 工具铁律"为完整系统提示。
func buildSystemPrompt(persona string) string {
	if persona == "" {
		persona = defaultPersona
	}
	return persona + toolRules
}

// maxHistory 限制保留的对话轮数，防止上下文无限增长。
const maxHistory = 20

// closeHardLimit 是退出时等 bot.Close() 的硬上限。取 8s：Close() 内部的优雅宽限是 3s
// （acquireGraceful，等当前帧走完），留足余量后仍不返回，就是设备已经不在、libusb 收不了尾了。
const closeHardLimit = 8 * time.Second

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
	// 关设备【必须有硬上限】，否则进程可能永远退不出来。实测(2026-07-17)：设备从 USB 总线上
	// 消失后 Close() 挂死——它内部两处都是无界等待(抢不到 d.mu 时的 d.mu.Lock()，以及 libusb 的
	// Close/Exit 在设备被意外拔除后不返回)。当时 server 已关、驱动已停、CPU 归零，进程却成了攥着
	// 句柄不放的僵尸，最后只能强杀。
	//
	// 超时后直接退出是安全的：能走到这里说明驱动已经收工（上面那个 defer 等过 driverDone），
	// 没有"半帧"可掐；而 Close() 都收不了尾，基本就是设备已经不在总线上了——真不在，也就无所谓
	// 打断谁。进程退出时 OS 会回收 USB 句柄。
	defer func() {
		done := make(chan struct{})
		go func() { defer close(done); _ = a.bot.Close() }()
		select {
		case <-done:
		case <-time.After(closeHardLimit):
			log.Warn("关闭设备超时，直接退出（设备多半已从总线消失、libusb 收不了尾；句柄交由 OS 回收）",
				"limit", closeHardLimit)
		}
	}()

	// Ctrl+C 触发优雅关闭。/api/shutdown（仅本机可调）走的也是这个 stop——强杀进程会把固件
	// 掐死在半帧的 lockstep 里、只能拔电源，所以必须有一条不依赖终端的干净退出路径。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a.shutdown = stop

	// 设备驱动循环【最先启动】：真机有“连接后就绪窗口”，必须连上(connectRobot)后尽快
	// 开始 Sync，否则窗口过期、设备就不再发就绪包(帧首读一直超时)。把它排在 speech/gesture
	// 连 sidecar 等耗时操作之前，缩短 连接→首帧 的间隔。
	driverDone := make(chan struct{})
	go func() { defer close(driverDone); a.driver.Run(ctx) }()
	// 退出时【先等驱动把手上这一帧 Sync 走完】，再让上面的 defer a.bot.Close() 关设备。
	// syncLoop 是"同步完一帧才检查 ctx"，所以等它返回 = 这帧完整。半路掐断传输会让固件卡在
	// lockstep 中间永久自旋、主控硬死（只能断电复位）——对照官方 Disconnect() 里的
	// syncTaskHandle.join()。defer 后进先出：本 defer 晚于 defer a.bot.Close() 注册，故先于它执行。
	defer func() {
		select {
		case <-driverDone:
		case <-time.After(5 * time.Second):
			log.Warn("驱动循环未按时收工，将强制关闭设备（可能打断传输，设备或需断电复位）")
		}
	}()

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
	srv    *server.Server
	chor   *choreography.Engine
	llm    *llm.Router
	bot    robot.Transport
	driver *device.Driver        // 统一设备驱动（拥有 Sync）
	screen *display.Compositor   // 屏幕画面合成（摄像头 / 素材片 / 程序动画脸）
	clips  *display.ClipSource   // 离线表情素材片（支持界面上传/删除后热重载）
	face   display.FallbackFace  // 兜底脸（SDF/老程序脸）：切表情类型时要同步它的黑白/彩色风格
	camera *display.CameraSource // 摄像头采集（可为 nil）

	emotionsDir string     // 表情素材目录（与 config.json 同目录的 emotions/）
	matMu       sync.Mutex // 串行化素材上传/删除，避免并发改写同一情绪文件
	speech      speech.Service
	gesture     gesture.Service
	music       *music.Service
	mm          *minimax.Client      // MiniMax 多模态(图片/语音)客户端；nil=未配置
	netTTS      *netspeech.TTSClient // 网络 TTS(OpenAI 兼容)；nil=未配置
	netASR      *netspeech.ASRClient // 网络 ASR(OpenAI 兼容)；nil=未配置
	player      *audioout.Player     // 设备侧 mp3 播放(mpg123 或 macOS playto)
	ttsToDevice bool                 // 配了 audio_device：TTS 始终定向设备喇叭(与 audio_out 解耦)
	robotStuck  atomic.Bool          // 设备疑似固件卡死(持续无就绪包)，广播给 UI
	robotRecovering atomic.Bool      // 卡死后正在自动串口软复位(免拔电源)自救中，供 UI 显示"自动复位中"而非"请断电"
	// shutdown 触发优雅退出（= signal.NotifyContext 的 stop，与 Ctrl+C 同一条路径）。
	// 由 main 注入；供 /api/shutdown 调用，见 handleShutdown 的注释——强杀会把固件掐死在半帧里。
	shutdown func()
	cameraOn    atomic.Bool          // 摄像头当前是否开启(屏幕显示摄像头画面)，上报 status 供前端同步开关
	wakeCache   map[string][]byte    // 唤醒人声应答缓存(短语→mp3)，第二次起秒回
	wakeMu      sync.Mutex
	wakeIdx     atomic.Uint32 // 轮换唤醒应答语
	genimg      *genImgStore  // 生成图暂存(供页面 HTTP 取回)
	genaudio    *genImgStore  // 生成音频(音乐)暂存(供页面 HTTP 取回)
	weather     *weather.Client

	speakMu     sync.Mutex         // 保护 speakCancel / turnCancel
	speakCancel context.CancelFunc // 取消进行中的一段语音(MiniMax 合成/设备播放)，用于打断 barge-in
	turnCancel  context.CancelFunc // 取消进行中的对话(LLM 流)；新一轮 handleChat 先打断上一轮，杜绝多轮并发交错、回复乱序

	asrMu         sync.Mutex  // 保护 asrPending / asrTimer
	asrPending    string      // 累积"一次说话被 VAD 切成的多段 ASR"
	asrTimer      *time.Timer // 防抖：最后一段后静默一会儿才发起【一次】对话，避免话没说完就调 LLM、触发限速
	audioStreamed atomic.Bool // 本轮是否已用小智流式音频播放（true 则收尾的 speak 不再重复合成）
	sched         *scheduler.Scheduler
	schedPath     string
	tools         *tools.Registry
	log           *slog.Logger

	rtMu      sync.Mutex        // 保护 rtSession
	rtSession *realtimeSession  // 当前实时语音会话（nil=未在会话中）；见 realtime.go
	rtBackend realtime.Backend  // 实时后端（qwen/glm），启动时按配置构造；nil=未启用实时

	frameSeq atomic.Uint32 // 屏幕镜像帧序号

	cfgMu   sync.Mutex     // 保护 cfg 的读改存
	cfg     *config.Config // 当前配置
	cfgPath string         // 配置文件路径

	qrMu    sync.Mutex     // 保护 qrLogin
	qrLogin *music.QRLogin // 进行中的 QQ 扫码登录会话

	actionsPath string       // 录制动作的存盘路径
	poseMu      sync.Mutex   // 保护 desiredPose
	desiredPose robot.Joints // 手动/跟随退出时保持的目标姿态

	recMu     sync.Mutex              // 保护录制状态
	recording bool                    // 是否正在录制
	recName   string                  // 当前录制动作名
	recFrames []choreography.Keyframe // 已采集的关键帧
	recStart  time.Time               // 录制起点

	histMu  sync.Mutex
	history []llm.Message // 对话历史（含系统提示）
	msgSeq  atomic.Uint64 // 对话消息 ID 自增源
}

// newApp 组装所有依赖。
func newApp(cfg *config.Config, cfgPath string, log *slog.Logger) (*app, error) {
	actionsPath := filepath.Join(filepath.Dir(cfgPath), "actions.json")

	// 小智设备身份持久化：device_id/client_id 为空则首次生成并写回配置，
	// 否则每次重启都换新身份 → xiaozhi.me 当成新设备反复要求绑定。
	idDirty := false
	for i := range cfg.Models {
		if cfg.Models[i].Type != "xiaozhi" {
			continue
		}
		if cfg.Models[i].DeviceID == "" {
			cfg.Models[i].DeviceID = xiaozhi.NewDeviceID()
			idDirty = true
		}
		if cfg.Models[i].ClientID == "" {
			cfg.Models[i].ClientID = xiaozhi.NewClientID()
			idDirty = true
		}
	}
	if idDirty {
		if err := cfg.Save(cfgPath); err != nil {
			log.Warn("保存小智设备身份失败", "err", err)
		} else {
			log.Info("已为小智生成并保存固定设备身份（device_id/client_id），后续重启不再要求重新绑定")
		}
	}

	// 大模型：从配置构建路由（至少含 Echo 兜底）。
	router := llm.NewRouter()
	var xzProviders []*llm.XiaozhiProvider
	for _, mc := range cfg.Models {
		p := providerFromConfig(mc)
		if xp, ok := p.(*llm.XiaozhiProvider); ok {
			xzProviders = append(xzProviders, xp)
		}
		router.Add(p)
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
		speech:      voice,
		gesture:     ges,
		log:         log,
		cfg:         cfg,
		cfgPath:     cfgPath,
		actionsPath: actionsPath,
		history:     []llm.Message{{Role: llm.RoleSystem, Content: buildSystemPrompt(cfg.Persona)}},
	}

	// 小智 Provider：按 persona_source 决定是否把本机角色注入小智（model=用小智自带角色）；
	// 并挂上流式音频回调——小智每说完一句就即时播放该句音频（边收边播，降低首声延迟）。
	for _, xp := range xzProviders {
		xp.SetPersonaFn(func() string {
			a.cfgMu.Lock()
			defer a.cfgMu.Unlock()
			if a.cfg.PersonaSource == "model" {
				return ""
			}
			return a.cfg.Persona
		})
		xp.SetAudioSink(func(ogg []byte) { a.streamXiaozhiAudio(ogg) })
	}

	// 新客户端连上时，推送一次状态快照，让前端立即渲染。
	a.srv = server.New(server.Options{
		Logger:   log,
		StaticFS: web.FS(),         // 内嵌前端单页应用，访问 http://addr/ 即是界面
		Routes:   a.materialRoutes, // 素材管理 REST 接口（上传/缩略图）
		OnConnect: func(c *server.Client) {
			if err := c.Send(a.statusSnapshot()); err != nil {
				log.Warn("推送初始状态失败", "err", err)
			}
			_ = c.Send(a.scheduleListEvent()) // 推送当前定时任务列表
			_ = c.Send(a.materialsEvent())    // 推送当前屏幕表情素材列表
			// 刷新/重连后恢复音乐播放状态（后端为准）：当前有曲且未停就把曲目+进度推回去。
			if pb := a.music.Snapshot(); pb.State != "" && pb.State != "stopped" && pb.Track.URL != "" {
				_ = c.Send(protocol.MusicEvent{
					State: string(pb.State), Name: pb.Track.Name, Artist: pb.Track.Artist,
					URL: pb.Track.URL, Position: pb.Position, Restore: true,
				})
			}
		},
	})

	// 语音 sidecar 连接状态变化（自动重连连上/断开）时，广播一次状态快照刷新界面。
	//
	// 【这个回调里绝不能给 sidecar 发消息】：它由 notify() 触发，而 sidecar 对多数命令都会回一条
	// 状态上报、上报又会触发 notify —— 在这里发命令就是自己喂自己的无限乒乓。曾把"下发已保存音色"
	// 写在这儿，实测 61 万次往返 / 86MB 日志，广播风暴还把浏览器打成"已断开·重连中"。
	// 需要"每次连上做一次"的事，交给 speech 包在【连接建立】那一刻做（见 SetDesiredVoice）。
	if sc, ok := a.speech.(*speech.Sidecar); ok {
		// 音色：主程序侧配置才是事实来源，sidecar 自己 config.json 里那份只是它的开机默认。
		// 记一次即可，之后每条新连接（含 sidecar 后启动、中途重连）都会自动下发。
		if v := cfg.Speech.Voice; v.SpeakerIDOr() >= 0 || v.Speed > 0 {
			sc.SetDesiredVoice(v.SpeakerIDOr(), v.Speed)
		}
		sc.OnStateChange(func() {
			if a.srv != nil {
				a.srv.Broadcast(a.statusSnapshot())
			}
		})
	}

	// 画面源：摄像头(可选) + 离线素材片 + 实时动画脸(眨眼/口型, 兜底)。
	// 统一设备驱动以固定帧率把"姿态 + 画面"一并 Sync 给设备，并把同一帧广播给 UI，实现镜像同步。
	// 兜底脸按 config.face 选：sdf(SDF 实时表情,默认,边缘平滑、情绪连续 morph) | classic(老程序脸)。
	var face display.FallbackFace
	if cfg.FaceOr() == "classic" {
		face = display.NewEmotionSource()
	} else {
		face = display.NewSDFFaceSource()
	}
	log.Info("表情脸", "type", cfg.FaceOr())
	a.emotionsDir = filepath.Join(filepath.Dir(cfgPath), "emotions")
	// 首次运行把内置默认表情播种到 emotions/（一生一次，尊重用户后续删除）。
	if n, err := display.SeedDefaultEmotions(a.emotionsDir); err != nil {
		log.Warn("写入内置默认表情失败", "err", err)
	} else if n > 0 {
		log.Info("已写入内置默认表情", "数量", n)
	}
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
	a.face = face
	a.screen = display.NewCompositor(a.camera, a.clips, face)
	// 表情类型（"系列"，见 config.FaceStyle）：
	//   类型B(b，默认)  = 关掉 GIF 素材层，全部情绪走 SDF 程序脸（否则素材会盖住 SDF 脸）。
	//   黑白眼睛类(bw) = 开启素材层，有 GIF 的用 GIF（官方黑底白眼那套），没有的自动用 SDF 补。
	// 两者只差这一个开关；可在运行时切换（handleSetFaceStyle）。
	a.applyFaceStyle(cfg.FaceStyleOr())
	log.Info("表情类型", "style", cfg.FaceStyleOr())
	// 机器人连接放到【最后一刻】：真机有“连接后就绪窗口”，连上后必须尽快开始 Sync，否则
	// 窗口过期、设备不再发就绪包(帧首读超时)。前面的表情播种等慢初始化都已做完，这里连上后
	// 紧接着 NewDriver→newApp 返回→main 里 driver.Run(已排最前) 即首帧，间隔最短、稳命中窗口。
	bot := connectRobot(cfg, log)
	a.bot = bot
	a.driver = device.NewDriver(bot, a.screen, log, 30, a.onDriverFrame, a.onDriverJoints)
	a.driver.SetServoEnable(cfg.IO.ServoEnable)      // 舵机总开关：关时不上扭矩（可手动摆姿）
	a.driver.SetJointTrim(cfg.JointTrim)             // 机械零位补偿：让上层的 0 就是端正姿态
	a.driver.SetAutoReboot(cfg.IO.AutoRebootOr())    // 卡死时自动串口软复位（免拔电源，io.auto_reboot 缺省开）
	a.driver.SetDisabledAxes(cfg.IO.ServoDisable)    // 不驱动的坏舵机轴（发 NaN 让固件跳过；隔离堵转拖垮全体）
	if len(cfg.IO.ServoDisable) > 0 {
		log.Info("已排除不驱动的舵机轴（发 NaN 让固件跳过）", "axes", cfg.IO.ServoDisable)
	}
	a.driver.SetStuckHandler(func(stuck, recovering bool) { // 卡死/自愈/恢复 → 广播状态供 UI 显示
		a.robotStuck.Store(stuck)
		a.robotRecovering.Store(recovering)
		a.srv.Broadcast(a.statusSnapshot())
	})
	if !cfg.IO.ServoEnable {
		log.Info("舵机总开关 = 关（servo_enable=false）：不上扭矩，可手动摆姿")
	}
	if cfg.JointTrim != (robot.Joints{}) {
		log.Info("已应用关节零位补偿", "trim", cfg.JointTrim)
	}

	// 动作编排：把姿态写入驱动；注册内置动作并加载用户录制的动作（同名覆盖内置）。
	// 注入表情回调（用 previewEmotion：只切脸、不联动同名动作，避免递归），
	// 让带「表情轨道」的动作（如 dance）在踩到关键帧时即时变脸。
	a.chor = choreography.NewEngine(a.driver,
		choreography.WithLogger(log),
		choreography.WithEmotionSink(func(e string) { a.previewEmotion(e) }),
	)
	for _, act := range choreography.DefaultActions() {
		a.chor.Register(act)
	}
	if err := a.chor.LoadActions(actionsPath); err != nil {
		log.Warn("加载录制动作失败", "err", err)
	}

	// 音乐：按 music.source 选音源（qq | kuwo，默认 kuwo）+ mpg123 播放（子进程，无 cgo），
	// 状态变化广播给 UI。播放与「生成音乐」是两件事：播放失败直接提示，不自动转生成。
	var searcher music.Searcher
	switch cfg.Music.SourceOr() {
	case "qq":
		searcher = music.NewQQMusicSearcher(cfg.Music.QQ.Cookie, cfg.Music.QQ.UIN, cfg.Music.QQ.Key)
		log.Info("音乐音源 = QQ 音乐", "hasCookie", cfg.Music.QQ.Cookie != "" || cfg.Music.QQ.Key != "")
	default:
		searcher = music.NewKuwoSearcher()
		log.Info("音乐音源 = 酷我")
	}
	// 播放器：页面输出(audio_out=page)时音乐由浏览器 <audio> 直接播放事件里的 URL，
	// 后端用 Mock 播放器、完全不依赖 mpg123；设备输出(device/both)时才用 mpg123 子进程出声。
	var musicPlayer music.Player
	switch {
	case cfg.IO.AudioOutOr() == "page":
		musicPlayer = music.NewMockPlayer()
		log.Info("音乐播放 = 浏览器(page)，后端不使用 mpg123")
	case runtime.GOOS == "darwin" && cfg.IO.AudioDevice != "" && findAudioHelper("musicto") != "":
		// macOS 设备输出：用 musicto(NSSound 定向 USB 声卡)，与 TTS 的 playto 同源，无需 mpg123。
		musicPlayer = music.NewDevicePlayer(findAudioHelper("musicto"), cfg.IO.AudioDevice, log)
		log.Info("音乐播放 = 设备(musicto 定向 USB 声卡)", "device", cfg.IO.AudioDevice)
	default:
		musicPlayer = music.NewMpg123Player(cfg.Music.Mpg123, log)
	}
	a.music = music.NewService(
		searcher,
		musicPlayer,
		log,
		func(t music.Track, st music.State) {
			a.srv.Broadcast(protocol.MusicEvent{State: string(st), Name: t.Name, Artist: t.Artist, URL: t.URL})
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

	// MiniMax 多模态（图片/语音）：凭据复用 models 里的 minimax 条目（或 io.minimax 显式配置）。
	if mmc := cfg.ResolveMiniMax(); mmc.BaseURL != "" && mmc.APIKey != "" {
		a.mm = minimax.NewWithGroup(mmc.BaseURL, mmc.APIKey, mmc.GroupID)
		log.Info("已启用 MiniMax 多模态", "base", mmc.BaseURL, "tts_engine", cfg.IO.TTSEngineOr(), "audio_out", cfg.IO.AudioOutOr(), "image_out", cfg.IO.ImageOutOr(), "group", a.mm.GroupID())
	}
	// 网络语音（OpenAI 兼容）：配置了 base_url 即启用，供 tts_engine=openai / audio_in=network 使用。
	if t := cfg.Speech.TTS; t.BaseURL != "" {
		a.netTTS = netspeech.NewTTS(t.BaseURL, t.APIKey, t.Model, t.Voice, t.Format)
		log.Info("已启用网络 TTS", "base", t.BaseURL, "model", t.Model)
	}
	if r := cfg.Speech.ASR; r.BaseURL != "" {
		a.netASR = netspeech.NewASR(r.BaseURL, r.APIKey, r.Model)
		log.Info("已启用网络 ASR", "base", r.BaseURL, "model", r.Model)
	}
	// 设备侧音频播放：macOS 上配了 audio_device 且找得到 playto helper 时，把 TTS 定向到该
	// USB 声卡（不动系统默认输出）；否则复用 mpg123 播到系统默认输出。
	if dev := cfg.IO.AudioDevice; runtime.GOOS == "darwin" && dev != "" {
		if playto := findPlaytoHelper(); playto != "" {
			a.player = audioout.NewCommand(playto, []string{dev}, log)
			a.ttsToDevice = true // 与 audio_out 解耦：TTS 始终从设备喇叭出
			log.Info("设备音频 = playto helper（定向 USB 声卡，不动系统默认）", "device", dev, "helper", playto)
		} else {
			a.player = audioout.New(cfg.Music.Mpg123, log)
			log.Warn("配了 audio_device 但未找到 playto helper，回退 mpg123", "device", dev)
		}
	} else {
		a.player = audioout.New(cfg.Music.Mpg123, log)
	}
	a.applyDeviceVolume(cfg.IO.AudioDevice, cfg.IO.DeviceVolumeOr()) // 启动即按配置设好设备音量
	a.genimg = newGenImgStore()
	a.genaudio = newGenImgStore()

	// 工具：注册可供大模型调用的工具（设备控制 / 情绪 / 动作 / 信息 / 音乐 / 天气 / 提醒 / 图片）。
	a.tools = buildTools(a)

	// 实时语音后端（可选）：配置了 realtime.enabled 且带 key 时构造。唤醒后建云端会话，
	// 麦克风原始音频上云、云端回音直播到设备喇叭、函数调用回本地 tools 执行。
	a.rtBackend = buildRealtimeBackend(cfg.Realtime)
	if a.rtBackend != nil {
		log.Info("已启用实时语音对话", "provider", cfg.Realtime.Provider, "model", cfg.Realtime.Model)
	}
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
// 走 BroadcastLossy：这是 10 次/秒的"最新即覆盖"实时量，慢连接上丢旧值无损——若按不可丢消息
// 处理，它会在镜像帧积压时把客户端顶爆、导致网页被当成慢连接踢掉（表现为镜像卡死）。
func (a *app) onDriverJoints(j robot.Joints) {
	a.srv.BroadcastLossy(protocol.JointsEvent{Angles: j, Enabled: true})
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
		// 只【探测】设备在不在（列设备，不 open/claim）；真正的 Connect 交给驱动循环，由它
		// 在连上后【零间隔】立刻 Sync 抢就绪窗口。探测不到则回退 Mock（无真机也能跑）。
		found, err := electronbot.ProbeErr()
		switch {
		case found:
			log.Info("已探测到 ElectronBot，连接交由驱动循环")
			dev := electronbot.New(log)
			dev.SetResetPort(cfg.IO.ResetPortOr()) // 串口软复位通道（免拔电源）：""/"auto" 自动找 CP210x/CH340
			return dev
		case err != nil:
			// libusb 没装 ≠ 设备没插。这两种情况以前打的是同一句"未探测到"，
			// 于是"机器人插着却连不上"要靠猜。见 docs/ELECTRONBOT.md。
			log.Warn("libusb 加载失败，真机无法连接，回退 Mock（Windows 上把 libusb-1.0.dll 放到工作目录；run.ps1 会自动下载）",
				"err", err)
		case mode == "electronbot":
			log.Warn("指定 electronbot 但未探测到设备（libusb 正常，总线上没有它）——检查是否插好、驱动是否为 WinUSB")
		default:
			log.Info("未探测到 ElectronBot，使用 Mock")
		}
	}

	m := robot.NewMock(log)
	_ = m.Connect()
	return m
}

// findPlaytoHelper 定位 macOS 的 playto 音频 helper（把声音定向到指定 USB 声卡）。
// 依次找：可执行文件同级的 sidecars/audio/playto、当前目录下的同路径、最后 PATH 里的 playto。
// 找不到返回空串。
func findPlaytoHelper() string           { return findAudioHelper("playto") }
func findAudioHelper(name string) string { return findHelper("audio", name) }
func findCamcapHelper() string           { return findHelper("video", "camcap") }

// findHelper 在 exe 同级 / 工作目录的 sidecars/<subdir>/ 下、再 PATH 里查找原生小工具
// （audio: playto/musicto；video: camcap）。找不到返回空串，调用方回退。
func findHelper(subdir, name string) string {
	rel := filepath.Join("sidecars", subdir, name)
	var cands []string
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), rel))
	}
	cands = append(cands, rel) // 相对当前工作目录（go run 时仓库根）
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// applyDeviceVolume 设置设备扬声器音量(0~100)。macOS + 配了 audio_device 时，经 playto
// 以空音频方式只设 USB 声卡输出音量后退出；其它平台/未配设备则无操作。
func (a *app) applyDeviceVolume(device string, vol int) {
	if runtime.GOOS != "darwin" || device == "" {
		return
	}
	playto := findAudioHelper("playto")
	if playto == "" {
		return
	}
	if vol < 0 {
		vol = 0
	} else if vol > 100 {
		vol = 100
	}
	cmd := exec.Command(playto, device, strconv.Itoa(vol)) // Stdin=nil → 读到空 → 只设音量后退出
	if err := cmd.Run(); err != nil {
		a.log.Warn("设置设备音量失败", "err", err, "vol", vol)
	}
}

// supportedEmotions 是机器人支持的【全部情绪】，单一事实来源：
//   - 提供给大模型的 set_emotion 工具枚举（buildTools）；
//   - 素材页列出的全部情绪（materialsEvent）——没有 GIF 素材的走程序脸(SDF)，缩略图实时渲染。
//
// 新增情绪时改这里 + internal/display/sdfface.go 的 emotionTarget/emotionColor（画法与配色），
// 想让它能被对话内容自动触发再加 realtime.go 的 emotionKeywords。
var supportedEmotions = []string{
	"neutral", "happy", "laughing", "lol", "funny", "sad", "angry", "crying", "loving",
	"naughty", "embarrassed", "surprised", "shocked", "scared", "thinking", "cool", "confident",
	"sleepy", "asleep", "tired", "winking", "confused", "speechless", "silly", "manic",
}

// applyFaceStyle 应用表情类型（"系列"）：
//   - bw 黑白眼睛类：开启 GIF 素材层——有素材的情绪用素材（官方黑底白眼那套），
//     该类型没有的情绪（色色/鬼畜/睡着了…）由兜底的 SDF 脸自动补上，不会做不出表情。
//   - b  类型B：关掉素材层，全部情绪由 SDF 程序脸实时绘制。
//
// compositor 本就是"有素材用素材、否则用兜底脸"，所以两个类型只差这一个开关；
// SetClipsEnabled 内部会 Invalidate 兜底脸，切换后立刻重画、不留残帧。
func (a *app) applyFaceStyle(style string) {
	bw := style == "bw"
	// 兜底脸也要跟着换风格：黑白眼睛类下，它补的那些没素材的情绪必须也是黑白，
	// 否则设备屏上会混进类型B的彩色脸、与同类型的 GIF 素材风格打架。
	if mf, ok := a.face.(display.MonoFace); ok {
		mf.SetMono(bw)
	}
	a.screen.SetClipsEnabled(bw)
}

// handleSetFaceStyle 运行时切换表情类型：校验 → 热生效 → 落盘 → 广播（各端同步高亮）。
func (a *app) handleSetFaceStyle(style string) {
	if !config.ValidFaceStyle(style) {
		a.log.Warn("非法表情类型", "style", style)
		return
	}
	a.cfgMu.Lock()
	a.cfg.FaceStyle = style
	a.saveConfig()
	a.cfgMu.Unlock()
	a.applyFaceStyle(style)
	a.log.Info("表情类型已切换", "style", style)
	a.srv.Broadcast(a.statusSnapshot())
}

// voicePreviewText 是试听音色时说的默认句子：覆盖常见声母韵母与一个问句语调，
// 短到能快速连听十几个音色、又长到听得出音色差别。
const voicePreviewText = "你好呀，我是电子脑壳。今天心情不错，要不要陪我聊会儿天？"

// handleSetVoice 换 sidecar TTS 音色/语速。
//
// 不校验 SpeakerID 范围——有效范围取决于 sidecar 装了哪个模型（fanchen-C 有 187 个音色、
// Piper 只有 1 个），主程序无从判断，交由 sidecar 夹紧并回发 voice 上报，界面以那个为准。
//
// Preview=true 只换音色试听一句、不落盘：挑音色时要连听十几个，每个都写盘既慢又会把
// config.json 刷成一堆中间态。选定后前端再发一条 Preview=false 的落盘。
func (a *app) handleSetVoice(ctx context.Context, cmd protocol.SetVoiceCommand) {
	if err := a.speech.SetVoice(ctx, cmd.SpeakerID, cmd.Speed); err != nil {
		a.log.Warn("换音色失败", "err", err, "speaker_id", cmd.SpeakerID)
		a.srv.Broadcast(protocol.ErrorEvent{Message: "换音色失败：" + err.Error()})
		return
	}
	if cmd.Preview {
		text := cmd.PreviewText
		if text == "" {
			text = voicePreviewText
		}
		// 试听走和正常说话同一条路（sidecar 合成 → 设备喇叭），听到的就是实际效果。
		if err := a.speech.Speak(ctx, text); err != nil {
			a.log.Warn("试听失败", "err", err)
		}
		return
	}
	a.cfgMu.Lock()
	sid := cmd.SpeakerID
	a.cfg.Speech.Voice.SpeakerID = &sid
	if cmd.Speed > 0 {
		a.cfg.Speech.Voice.Speed = cmd.Speed
	}
	speed := a.cfg.Speech.Voice.Speed
	a.saveConfig()
	a.cfgMu.Unlock()
	// 同步"每次连上要下发的音色"，否则 sidecar 一重连又会退回旧值。
	if sc, ok := a.speech.(*speech.Sidecar); ok {
		sc.SetDesiredVoice(sid, speed)
	}
	a.log.Info("音色已保存", "speaker_id", sid, "speed", speed)
	a.srv.Broadcast(a.statusSnapshot())
}

// isSupportedEmotion 报告 name 是否为支持的情绪（供素材页判断该不该渲染程序脸缩略图）。
func isSupportedEmotion(name string) bool {
	for _, e := range supportedEmotions {
		if e == name {
			return true
		}
	}
	return false
}

// buildTools 构造工具注册表，副作用以闭包注入（情绪/动作走机器人，台灯为内置设备）。
func buildTools(a *app) *tools.Registry {
	reg := tools.NewRegistry()

	reg.Register(tools.EmotionTool(supportedEmotions, func(e string) error {
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
			a.log.Warn("播放音乐失败", "query", query, "err", err)
			if errors.Is(err, music.ErrRateLimited) {
				// 限流是暂时的，且与登录/版权无关——别再误导成"需登录"。
				return "", fmt.Errorf("《%s》暂时没放成：音源正忙(限流)，过一会儿再试。这不是登录或版权问题。", query)
			}
			return "", fmt.Errorf("没能播放《%s》：%w（在线音源可能无版权或需登录，可改用「生成音乐」）", query, err)
		}
		return fmt.Sprintf("正在播放：%s - %s", t.Name, t.Artist), nil
	}))
	// 音乐控制：换一首/下一首/上一首/暂停/继续/停止（对当前播放列表操作，不重新搜索）。
	reg.Register(tools.MusicControlTool(func(ctx context.Context, action string) (string, error) {
		switch action {
		case "next":
			t, err := a.music.Next(ctx)
			if err != nil {
				return "", fmt.Errorf("换歌失败：%w（先放一首再换）", err)
			}
			return fmt.Sprintf("已换到：%s - %s", t.Name, t.Artist), nil
		case "prev":
			t, err := a.music.Prev(ctx)
			if err != nil {
				return "", fmt.Errorf("切上一首失败：%w", err)
			}
			return fmt.Sprintf("已切到上一首：%s - %s", t.Name, t.Artist), nil
		case "pause":
			a.music.Pause()
			return "已暂停", nil
		case "resume":
			a.music.Resume()
			return "继续播放", nil
		case "stop":
			a.music.Stop()
			return "已停止", nil
		default:
			return "", fmt.Errorf("未知操作：%s", action)
		}
	}))

	// 视觉："看一眼"——抓摄像头帧交给视觉模型描述（仅在配置了摄像头时提供）。
	if a.camera != nil {
		reg.Register(tools.LookTool(a.captureJPEG, func(ctx context.Context, jpeg []byte, q string) (string, error) {
			return a.llm.Vision(ctx, jpeg, q)
		}))
	}

	// 文生图 / 音乐生成：配置了 MiniMax 才提供。
	if a.mm != nil {
		reg.Register(tools.ImageTool(func(ctx context.Context, prompt string) (string, error) {
			return a.handleGenerateImage(ctx, prompt)
		}))
		reg.Register(tools.MusicGenTool(func(ctx context.Context, prompt, lyrics string) (string, error) {
			return a.handleGenerateMusic(ctx, prompt, lyrics)
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
			VideoSize: cfg.VideoSize, Framerate: cfg.Framerate,
			Backend: cfg.Backend, Camcap: findCamcapHelper(),
			Rotate: cfg.Rotate, Mirror: cfg.Mirror,
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
	case "xiaozhi":
		name := mc.Name
		if name == "" {
			name = "小智"
		}
		wsURL := mc.WSURL
		if wsURL == "" {
			wsURL = mc.BaseURL // 兼容用 base_url 填 ws 地址
		}
		return llm.NewXiaozhi(id, name, xiaozhi.Config{
			WSURL: wsURL, OTAURL: mc.OTAURL, Token: mc.Token, DeviceID: mc.DeviceID, ClientID: mc.ClientID,
		}, nil)
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
// 仅当 audio_in=device（设备麦经 sidecar）时采纳；audio_in=page/off 时忽略 sidecar 输入，
// 避免与网页麦克风输入重复。
func (a *app) handleSpeechEvent(ctx context.Context, ev speech.Event) {
	if a.cfg.IO.AudioInOr() != "device" {
		return
	}
	// 实时模式：唤醒即建云端会话，麦克风原始音频直接上云（云端做 VAD/ASR/LLM/TTS）。
	// 本地 ASR 那条路（KindASR→debounceChat）在此模式下不会触发——sidecar 收到 stream_start
	// 后就停发 asr、改发 audio，见 sidecar.py。
	if a.realtimeEnabled() {
		switch ev.Kind {
		case speech.KindWake:
			a.srv.Broadcast(protocol.WakeEvent{Keyword: ev.Keyword})
			a.startRealtimeSession(ctx) // 建立/续期会话
		case speech.KindVAD:
			a.srv.Broadcast(protocol.VADEvent{Speaking: ev.Speaking, Level: ev.Level})
		case speech.KindAudio:
			a.feedRealtimeAudio(ev.PCM) // 麦克风原始音频推给云端
		}
		return
	}

	switch ev.Kind {
	case speech.KindWake:
		a.srv.Broadcast(protocol.WakeEvent{Keyword: ev.Keyword})
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceListening})
		go a.speakWake(context.Background()) // 唤醒后人声应答("我在"等，带缓存秒回；无TTS退回提示音)

	case speech.KindVAD:
		a.srv.Broadcast(protocol.VADEvent{Speaking: ev.Speaking, Level: ev.Level})

	case speech.KindASR:
		// 把识别文本作为前端的中间/最终 ASR 展示。
		a.srv.Broadcast(protocol.ASREvent{Text: ev.Text, Final: ev.Final})
		// 最终结果触发对话：经防抖合并"一次说话被切成的多段"，避免话没说完就多次调 LLM。
		if ev.Final && ev.Text != "" {
			a.debounceChat(ctx, ev.Text)
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
		a.speech.Stop()                                  // 打断 sidecar 语音
		a.cancelSpeak()                                  // 取消进行中的 MiniMax 合成/设备播放
		a.player.Stop()                                  // 兜底再杀一次设备播放
		a.srv.Broadcast(protocol.AudioEvent{Stop: true}) // 让页面停止播放
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

	case protocol.TypeParty:
		if cmd, err := protocol.As[protocol.PartyCommand](in.Env); err == nil {
			go a.handleParty(ctx, cmd.Query) // 一键蹦迪：放歌 + 跳舞，放后台
		}

	case protocol.TypeReenable:
		a.driver.Reenable() // 舵机过载保护锁存后手动解锁（驱动也会自动检测并重试）

	case protocol.TypeSetFaceStyle:
		if cmd, err := protocol.As[protocol.SetFaceStyleCommand](in.Env); err == nil {
			a.handleSetFaceStyle(cmd.Style)
		}

	case protocol.TypeSetVoice:
		if cmd, err := protocol.As[protocol.SetVoiceCommand](in.Env); err == nil {
			a.handleSetVoice(ctx, cmd)
		}

	case protocol.TypeRebootDevice:
		go func() { // 串口软复位阻塞 ~1s 且设备会掉线重连，放后台；成功/失败都由状态广播与日志反映
			if err := a.driver.Reboot(); err != nil {
				a.log.Warn("手动软复位失败", "err", err)
			}
		}()

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

	case protocol.TypeSetIO:
		if cmd, err := protocol.As[protocol.SetIOCommand](in.Env); err == nil {
			a.handleSetIO(cmd)
		}

	case protocol.TypeSetDevice:
		if cmd, err := protocol.As[protocol.SetDeviceCommand](in.Env); err == nil {
			a.handleSetDevice(cmd)
		}

	case protocol.TypeSetVolume:
		if cmd, err := protocol.As[protocol.SetVolumeCommand](in.Env); err == nil {
			a.handleSetVolume(cmd)
		}

	case protocol.TypeSetRealtime:
		if cmd, err := protocol.As[protocol.SetRealtimeCommand](in.Env); err == nil {
			go a.handleSetRealtime(cmd) // 可能停会话+关云端连接，放后台避免阻塞分发循环
		}

	default:
		a.log.Debug("未处理的命令类型", "type", in.Env.Type)
	}
}

// handleChat 是对话入口：当生效模型支持工具调用且已注册工具时走工具循环，
// 否则走流式对话。
// debounceChat 合并"一次说话被 VAD 切成的多段 ASR"：累积文本，在最后一段后静默 ~600ms 才
// 发起【一次】对话。避免话没说完(VAD 中途断句)就提前、多次调用 LLM，从而频繁触发接口限速。
func (a *app) debounceChat(ctx context.Context, text string) {
	const wait = 600 * time.Millisecond
	a.asrMu.Lock()
	defer a.asrMu.Unlock()
	if a.asrPending == "" {
		a.asrPending = text
	} else {
		a.asrPending += text // 续上前一段（同一句被切开）
	}
	if a.asrTimer != nil {
		a.asrTimer.Stop()
	}
	a.asrTimer = time.AfterFunc(wait, func() {
		a.asrMu.Lock()
		t := a.asrPending
		a.asrPending = ""
		a.asrMu.Unlock()
		if t != "" {
			a.handleChat(ctx, t)
		}
	})
}

func (a *app) handleChat(ctx context.Context, text string) {
	if text == "" {
		return
	}
	// 新一轮对话：先打断上一轮（LLM 流 + 正在合成/播放的 TTS），保证一次只处理一轮，
	// 杜绝你连说几句时多轮并发、回复碎片交错乱序（"对话没有时序"的根因）。
	a.speakMu.Lock()
	if a.turnCancel != nil {
		a.turnCancel()
	}
	if a.speakCancel != nil {
		a.speakCancel()
		a.speakCancel = nil
	}
	tctx, cancel := context.WithCancel(ctx)
	a.turnCancel = cancel
	a.speakMu.Unlock()

	// 确定性快捷路径：音乐控制口令（换一首/暂停/继续/停…）直接执行，不经过大模型，
	// 保证 100% 生效（大模型对这类指令偶尔会只口头回应而不调工具）。
	if a.tryMusicShortcut(tctx, text) {
		return
	}
	if a.tools.Count() > 0 && a.llm.ActiveSupportsTools() {
		a.handleChatWithTools(tctx, text)
	} else {
		a.handleChatStreaming(tctx, text)
	}
}

// musicIntents 把纯控制口令映射到动作。仅匹配"整句就是这个口令"的情况，
// 含具体歌名（如"换一首周杰伦"）不会命中，仍交给大模型走 play_music。
var musicIntents = map[string]string{
	"换一首": "next", "换首": "next", "换首歌": "next", "换一首歌": "next", "换歌": "next",
	"下一首": "next", "下一曲": "next", "下首": "next", "切歌": "next", "换个": "next",
	"换一个": "next", "换个歌": "next", "再来一首": "next", "再换一首": "next", "换": "next",
	"上一首": "prev", "上一曲": "prev", "上首歌": "prev", "前一首": "prev",
	"暂停": "pause", "暂停一下": "pause", "暂停播放": "pause", "停一下": "pause", "先暂停": "pause",
	"继续": "resume", "继续播放": "resume", "接着放": "resume", "继续放": "resume",
	"停止": "stop", "停止播放": "stop", "别放了": "stop", "关掉音乐": "stop",
	"关音乐": "stop", "关闭音乐": "stop", "不听了": "stop", "停掉": "stop",
}

// musicPlayPrefixes 是"放歌"意图的句首动词；命中则把其后的内容当作搜索词。
// 不含裸"放/听"（误命中"放假/想听你说话"太多），只收明确的放歌句式。
var musicPlayPrefixes = []string{
	"播放", "放一首歌", "放一首", "放首歌", "放首", "放点", "放个", "放歌",
	"我想听听", "我想听", "我要听", "想听", "要听",
	"听一首", "听首", "听点", "听个",
	"来一首", "来首", "来点", "来个",
	"唱一首", "唱首", "点一首", "点首",
}

// genericMusicWords 是"放点音乐"这类不含具体歌名的泛化请求，命中则放热门。
var genericMusicWords = map[string]bool{
	"": true, "音乐": true, "歌": true, "歌曲": true, "首歌": true,
	"点音乐": true, "点歌": true, "随便": true, "随便一首": true,
}

// parseMusicPlay 解析"放歌"口令：命中返回搜索词（泛化请求返回"热门"）与 true。
// 含疑问词的句子（"播放列表怎么做"）不当作放歌，避免误劫持正常对话。
func parseMusicPlay(text string) (string, bool) {
	t := strings.TrimSpace(text)
	// 去掉句首语气/请求填充词（"帮我随便来首歌" → "来首歌"），便于后面匹配放歌动词。
	for changed := true; changed; {
		changed = false
		for _, f := range []string{"帮我", "帮忙", "给我", "麻烦", "随便", "请", "我"} {
			if strings.HasPrefix(t, f) {
				t = strings.TrimSpace(t[len(f):])
				changed = true
			}
		}
	}
	for _, p := range musicPlayPrefixes {
		if !strings.HasPrefix(t, p) {
			continue
		}
		q := strings.TrimSpace(t[len(p):])
		for _, q2 := range []string{"歌曲", "音乐", "这首歌", "首歌", "的歌", "歌"} {
			q = strings.TrimSuffix(strings.TrimSpace(q), q2)
		}
		q = strings.Trim(q, "的吧呀啊呗嘛~。.!！,，、 \t")
		// 疑问/讨论句不当作放歌。
		for _, bad := range []string{"怎么", "为什么", "是什么", "吗", "?", "？"} {
			if strings.Contains(q, bad) {
				return "", false
			}
		}
		if genericMusicWords[q] {
			return "热门", true
		}
		return q, true
	}
	return "", false
}

// tryMusicShortcut 命中音乐控制/放歌口令则直接执行并返回 true；否则返回 false 走正常对话。
// 这条确定性路径不经过大模型，因此小智(xiaozhi)等不支持 function-calling 的后端也能放歌。
func (a *app) tryMusicShortcut(ctx context.Context, text string) bool {
	cleaned := strings.TrimRight(strings.TrimSpace(text), "吧呗啊呀嘛吗~。.!！,，、 \t")
	action, ok := musicIntents[cleaned]
	if !ok {
		// 不是控制词，再看是不是"放某首歌"的意图。
		if q, ok := parseMusicPlay(text); ok {
			a.emitUser(text)
			msg := "正在为你播放：" + q
			a.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: msg})
			a.srv.Broadcast(protocol.ChatEvent{ID: a.nextMsgID(), Role: protocol.RoleAssistant, Content: msg, Status: protocol.ChatFinal})
			a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
			go a.handleMusic(ctx, protocol.MusicCommand{Action: "play", Query: q})
			a.log.Info("放歌快捷指令", "text", text, "query", q)
			return true
		}
		return false
	}
	// next/prev 需要已有播放列表，否则交给大模型去搜一首播。
	if (action == "next" || action == "prev") && !a.music.HasPlaylist() {
		return false
	}
	a.emitUser(text)
	var msg string
	switch action {
	case "next":
		if t, err := a.music.Next(ctx); err == nil {
			msg = "已换一首：" + t.Name + " - " + t.Artist
		} else {
			msg = "暂时换不了歌：" + err.Error()
		}
	case "prev":
		if t, err := a.music.Prev(ctx); err == nil {
			msg = "已切上一首：" + t.Name + " - " + t.Artist
		} else {
			msg = "暂时切不了：" + err.Error()
		}
	case "pause":
		a.music.Pause()
		msg = "已暂停"
	case "resume":
		a.music.Resume()
		msg = "继续播放"
	case "stop":
		a.music.Stop()
		msg = "已停止播放"
	}
	a.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: msg})
	a.srv.Broadcast(protocol.ChatEvent{ID: a.nextMsgID(), Role: protocol.RoleAssistant, Content: msg, Status: protocol.ChatFinal})
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
	a.log.Info("音乐控制快捷指令", "text", text, "action", action)
	return true
}

// emitUser 记录并回显一条用户消息，随后进入思考态。
func (a *app) emitUser(text string) {
	a.audioStreamed.Store(false) // 新一轮开始：清空"已流式播放音频"标记
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
	a.finishReply(ctx, content, true)
}

// finishReply 完成一条助手回复。speakIt=false 时只入历史/显示、不朗读——
// 用于"放歌/切歌"这类已经有音频输出的轮次，避免确认语音打断刚开始的音乐。
func (a *app) finishReply(ctx context.Context, content string, speakIt bool) {
	if content != "" {
		a.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: content})
		// 自动定表情，让脸跟对话走（模型常不主动调 set_emotion）。优先用模型自带情绪(小智服务端
		// 下发，最准)，没有再按文本关键词兜底。只在有明确情绪时改（中性不动），免得覆盖模型刚设的。
		// 放在 speak 之前 → 说话时脸已是对应表情，同步。
		emo := a.llm.ActiveEmotion()
		if emo == "" {
			emo = sentimentEmotion(content)
		}
		if emo != "" && emo != "neutral" {
			a.setFaceEmotion(emo)
		}
		if speakIt {
			a.speak(ctx, content) // 按 io.tts_engine/audio_out 路由（MiniMax 云端 / sidecar；设备/页面）
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
	musicTurn := false // 本轮是否触发了音乐播放/切换（则不朗读确认，避免打断音乐）
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceSpeaking})
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStart})
	a.screen.SetSpeaking(true) // 驱动口型动画

	res, err := llm.RunToolLoop(ctx, a.llm, a.historySnapshot(), lt, a.tools.Execute, 5,
		func(ec llm.ExecutedCall) {
			if ec.Err == "" && (ec.Name == "play_music" || ec.Name == "generate_music" || ec.Name == "music_control") {
				musicTurn = true
			}
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
		reportLLMErr(a, ctx, "llm_error", err)
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
		return
	}

	content := stripThink(res.Content) // 剥掉推理模型的 <think> 块，再显示/朗读
	a.srv.Broadcast(protocol.ChatEvent{
		ID: id, Role: protocol.RoleAssistant, Content: content, Tools: pTools, Status: protocol.ChatFinal,
	})
	// 放歌/切歌轮次：只显示文字、不朗读，免得确认语音打断刚开始的音乐。
	a.finishReply(ctx, content, !musicTurn)
}

// handleChatStreaming 跑一次流式对话：用户消息回显 → 思考 → 流式生成 → 朗读。
func (a *app) handleChatStreaming(ctx context.Context, text string) {
	// 1) 记录并回显用户消息 + 进入思考态。
	a.emitUser(text)

	// 3) 流式生成助手回复。
	ch, err := a.llm.Chat(ctx, llm.Request{Messages: a.historySnapshot()})
	if err != nil {
		reportLLMErr(a, ctx, "llm_error", err)
		a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
		return
	}

	id := a.nextMsgID()
	var content string
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceSpeaking})
	a.srv.Broadcast(protocol.TTSEvent{State: protocol.TTSStart})
	a.screen.SetSpeaking(true) // 驱动口型动画

	// 流式 TTS：minimax host 合成且有输出时，边生成边【逐句】合成播放——话音紧跟，不必等整段
	// 生成完再开口（治"响应慢"）。其它引擎仍走收尾整段朗读。
	io := a.ioSnapshot()
	var sink *ttsSink
	if io.TTSEngine == "minimax" && a.mm != nil && io.AudioOut != "off" {
		sctx, cancel := context.WithCancel(context.Background())
		a.setSpeakCancel(cancel) // 纳入 barge-in：打断即停
		sink = a.newTTSSink(sctx)
	}
	spoken := 0          // 已送去朗读的 rune 数
	var ttsBuf string    // 攒句缓冲：第一句立刻播，后续攒够再合成，减少 TTS 调用次数防限速
	firstSpoken := false // 是否已播出第一句

	for chunk := range ch {
		if chunk.Err != nil {
			reportLLMErr(a, ctx, "llm_stream", chunk.Err)
			break
		}
		if chunk.Done {
			break
		}
		content += chunk.Delta
		clean := stripThink(content) // 剥掉 <think>，避免推理过程逐字泄漏到页面
		a.srv.Broadcast(protocol.ChatEvent{
			ID:      id,
			Role:    protocol.RoleAssistant,
			Content: clean,
			Status:  protocol.ChatStreaming,
		})
		if sink != nil && !insideThink(content) { // 抽出新出现的完整句
			var sents []string
			sents, spoken = takeSentences(clean, spoken)
			for _, s := range sents {
				if !firstSpoken { // 第一句立刻合成播放（话音紧跟）
					sink.push(s)
					firstSpoken = true
				} else { // 后续攒够 ~24 字再合成一次（减少调用、防限速）
					ttsBuf += s
					if len([]rune(ttsBuf)) >= 24 {
						sink.push(ttsBuf)
						ttsBuf = ""
					}
				}
			}
		}
	}

	content = stripThink(content)
	a.srv.Broadcast(protocol.ChatEvent{
		ID:      id,
		Role:    protocol.RoleAssistant,
		Content: content,
		Status:  protocol.ChatFinal,
	})
	if sink != nil {
		rest := ttsBuf // 残留攒句 + 无终止符的尾巴，一起补播
		if runes := []rune(content); spoken < len(runes) {
			rest += cleanForSpeech(string(runes[spoken:]))
		}
		if rest = strings.TrimSpace(rest); rest != "" {
			sink.push(rest)
		}
		sink.close()                       // 等所有句子合成播放完
		a.finishReply(ctx, content, false) // 入历史 + 收尾状态，不再整段朗读（已逐句播过）
	} else {
		a.finishAssistant(ctx, content)
	}
}

// handleJog 手动微调单个舵机：更新目标姿态并交给驱动下发（角度反馈由驱动统一广播）。
func (a *app) handleJog(cmd protocol.JogJointCommand) {
	if cmd.Joint < 0 || cmd.Joint >= robot.JointCount {
		a.log.Warn("舵机序号越界", "joint", cmd.Joint)
		return
	}
	a.poseMu.Lock()
	a.desiredPose[cmd.Joint] = robot.ClampAngle(cmd.Joint, cmd.Angle) // 限位，避免超出舵机安全范围
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
			msg := "没能播放《" + cmd.Query + "》：在线音源无版权或需登录"
			if errors.Is(err, music.ErrRateLimited) {
				// 限流 ≠ 没登录：给出准确、不误导的提示。
				msg = "《" + cmd.Query + "》暂时没放成：音源正忙(限流)，过一会儿再试(不是登录问题)"
			}
			a.srv.Broadcast(protocol.ErrorEvent{Code: "music_error", Message: msg})
		}
	case "pause":
		a.music.Pause()
	case "resume":
		a.music.Resume()
	case "stop":
		a.music.Stop()
	case "next": // 连续播放：上一首结束自动切，或用户手动切歌
		if _, err := a.music.Next(ctx); err != nil {
			a.log.Warn("切下一首失败", "err", err)
		}
	case "prev":
		if _, err := a.music.Prev(ctx); err != nil {
			a.log.Warn("切上一首失败", "err", err)
		}
	case "volume":
		a.music.SetVolume(cmd.Volume)
	case "report": // 页面上报播放进度/状态，后端记录以便刷新重连恢复
		a.music.SetProgress(cmd.Position, cmd.Playing)
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

// defaultPartyQuery 是「一键蹦迪」默认曲目（节奏感强）；搜不到则只跳舞。
const defaultPartyQuery = "最炫民族风"

// handleParty 一键蹦迪：放歌 + 无限循环跳 dance（踩拍变脸）。
// 停止由前端发 interrupt（停舞）+ music stop（停歌）完成。
//
// 【先出声、再起舞】：放歌要经过搜索→取播放地址→下载，少则一两秒。以前是立刻起舞、并发去放歌，
// 于是舞先跳、歌后响，整段听起来就是"音乐慢半拍"。现在等 SearchAndPlay 返回（此时音频已交给
// 播放器、马上出声）才起舞，起点对齐。放歌失败则退回只跳舞，不至于点了没反应。
func (a *app) handleParty(ctx context.Context, query string) {
	if query == "" {
		query = defaultPartyQuery
	}
	a.srv.Broadcast(protocol.ChatEvent{
		ID: a.nextMsgID(), Role: protocol.RoleAssistant,
		Content: "🪩 开跳！正在放《" + query + "》，点「打断」即可停。", Status: protocol.ChatFinal,
	})
	go func() {
		if _, err := a.music.SearchAndPlay(ctx, query); err != nil {
			a.log.Warn("蹦迪：放歌失败，仅跳舞", "query", query, "err", err)
		}
		if err := a.chor.Play(ctx, "dance", -1); err != nil {
			a.log.Warn("蹦迪：起舞失败", "err", err)
			a.srv.Broadcast(protocol.ErrorEvent{Code: "party", Message: "起舞失败：" + err.Error()})
		}
	}()
	a.log.Info("一键蹦迪", "query", query)
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
		// ScreenMode：摄像头到屏由设备同步 owner 串行拉帧(来一帧发一帧，仿官方单一串行发送)，
		// 不起并发读帧 goroutine——消除"并发读管道帧"卡死设备屏的路径。
		if err := a.camera.Start(ctx, display.CameraConfig{
			FFmpeg: cfg.FFmpeg, InputFormat: cfg.InputFormat, Input: cfg.Input,
			VideoSize: cfg.VideoSize, Framerate: cfg.Framerate,
			Backend: cfg.Backend, Camcap: findCamcapHelper(), ScreenMode: true,
			Rotate: cfg.Rotate, Mirror: cfg.Mirror,
		}); err != nil {
			a.log.Warn("启动摄像头失败", "err", err)
			a.srv.Broadcast(protocol.ErrorEvent{Code: "camera_error", Message: err.Error()})
			return
		}
		a.driver.SetFramePuller(a.camera.ReadFrame) // syncLoop 进入摄像头模式，串行拉帧→同步
		a.screen.SetCamera(true)
		a.cameraOn.Store(true)
	} else {
		a.driver.SetFramePuller(nil) // 先退出摄像头模式
		a.screen.SetCamera(false)
		a.camera.Stop() // 关管道解阻塞在途的 ReadFrame，syncLoop 回落表情脸
		a.cameraOn.Store(false)
	}
	a.srv.Broadcast(a.statusSnapshot()) // 广播新状态，前端开关据此同步开/关
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
			Stuck:      a.robotStuck.Load(),
			Recovering: a.robotRecovering.Load(),
			Speed:      a.bot.Speed(),
			VID:       0x1001, // ElectronBot 设备标识（Mock 下仅作展示）
			PID:       0x8023,
			FPS:       30,
		},
		ASR:      protocol.ServiceStatus{Running: ss.ASRRunning, Detail: ss.Detail},
		TTS:      protocol.ServiceStatus{Running: ss.TTSRunning, Detail: ss.Detail},
		// 音色能力来自 sidecar 上报（装 fanchen-C=187 个、装 Piper=1 个），不可写死。
		SidecarVoice: protocol.SidecarVoiceStatus{
			Speakers:  ss.Voice.Speakers,
			SpeakerID: ss.Voice.SpeakerID,
			Speed:     ss.Voice.Speed,
		},
		LLM:      protocol.LLMStatus{Active: a.llm.ActiveID(), Available: models},
		Actions:  actions,
		Camera:   a.camera != nil,
		CameraOn: a.cameraOn.Load(),
		IO: protocol.IOStatus{
			AudioIn:      a.cfg.IO.AudioInOr(),
			AudioOut:     a.cfg.IO.AudioOutOr(),
			TTSEngine:    a.cfg.IO.TTSEngineOr(),
			ImageOut:     a.cfg.IO.ImageOutOr(),
			DeviceVolume: a.cfg.IO.DeviceVolumeOr(),
			ServoEnable:  a.cfg.IO.ServoEnable,
		},
		Realtime: protocol.RealtimeStatus{
			Enabled:  a.cfg.Realtime.Enabled,
			Provider: a.cfg.Realtime.Provider,
			WSBase:   a.cfg.Realtime.WSBase,
			Model:    a.cfg.Realtime.Model,
			Voice:    a.cfg.Realtime.Voice,
			HasKey:   a.cfg.Realtime.APIKey != "", // 只报有没有，不回传 key 明文
		},
		Music:         protocol.MusicStatus{Source: a.cfg.Music.SourceOr(), LoggedIn: a.cfg.Music.SourceOr() == "qq" && a.cfg.Music.QQ.Cookie != ""},
		Persona:       a.cfg.Persona,
		PersonaSource: a.cfg.PersonaSourceOr(),
		Voice:         a.cfg.Voice,
		FaceStyle:     a.cfg.FaceStyleOr(),
	}
}

// validIO 校验某个 I/O 路由取值是否在允许集合内。
func validIO(field, v string) bool {
	switch field {
	case "audio_in":
		return v == "device" || v == "page" || v == "network" || v == "off"
	case "audio_out":
		return v == "device" || v == "page" || v == "both" || v == "off"
	case "tts_engine":
		return v == "minimax" || v == "sidecar" || v == "openai" || v == "xiaozhi"
	case "image_out":
		return v == "device" || v == "page" || v == "both" || v == "off"
	}
	return false
}

// handleSetIO 更新 I/O 路由配置（设置页）：校验后写入并落盘，广播新状态使各端同步。
func (a *app) handleSetIO(cmd protocol.SetIOCommand) {
	a.cfgMu.Lock()
	if cmd.AudioIn != "" && validIO("audio_in", cmd.AudioIn) {
		a.cfg.IO.AudioIn = cmd.AudioIn
	}
	if cmd.AudioOut != "" && validIO("audio_out", cmd.AudioOut) {
		a.cfg.IO.AudioOut = cmd.AudioOut
	}
	if cmd.TTSEngine != "" && validIO("tts_engine", cmd.TTSEngine) {
		a.cfg.IO.TTSEngine = cmd.TTSEngine
	}
	if cmd.ImageOut != "" && validIO("image_out", cmd.ImageOut) {
		a.cfg.IO.ImageOut = cmd.ImageOut
	}
	if cmd.ServoEnable != nil {
		a.cfg.IO.ServoEnable = *cmd.ServoEnable
	}
	a.saveConfig()
	a.cfgMu.Unlock()
	// 舵机总开关即时生效：驱动下一帧就按新值下发使能位。关→开时线上使能位自然产生
	// 0→1 跳变，固件据此给舵机上扭矩，无需额外补一次 Reenable 脉冲。
	if cmd.ServoEnable != nil && a.driver != nil {
		a.driver.SetServoEnable(*cmd.ServoEnable)
		a.log.Info("舵机总开关已切换", "on", *cmd.ServoEnable)
	}
	a.srv.Broadcast(a.statusSnapshot())
	// 输出路由切到 page/both 后，把当前曲目再次下发，让页面无需重新点歌即可创建
	// 可播放、可 seek 的 <audio> 元素。设备独播没有可读取的播放时钟，不能提供精确进度。
	if cmd.AudioOut != "" {
		if pb := a.music.Snapshot(); pb.State != "" && pb.State != music.StateStopped && pb.Track.URL != "" {
			a.srv.Broadcast(protocol.MusicEvent{
				State: string(pb.State), Name: pb.Track.Name, Artist: pb.Track.Artist,
				URL: pb.Track.URL, Position: pb.Position, Restore: true,
			})
		}
	}
}

// handleSetRealtime 更新实时语音对话配置（设置页）：落盘 → 结束当前会话 → 热重建后端 → 广播状态。
// 空/nil 字段表示不改动（api_key 尤其：状态里从不回传明文，空即保留原值，避免脱敏回显把它清空）。
func (a *app) handleSetRealtime(cmd protocol.SetRealtimeCommand) {
	a.cfgMu.Lock()
	if cmd.Enabled != nil {
		a.cfg.Realtime.Enabled = *cmd.Enabled
	}
	if cmd.Provider != "" {
		a.cfg.Realtime.Provider = cmd.Provider
	}
	if cmd.WSBase != "" {
		a.cfg.Realtime.WSBase = cmd.WSBase
	}
	if cmd.Model != "" {
		a.cfg.Realtime.Model = cmd.Model
	}
	if cmd.APIKey != "" {
		a.cfg.Realtime.APIKey = cmd.APIKey
	}
	if cmd.Voice != "" {
		a.cfg.Realtime.Voice = cmd.Voice
	}
	rtCfg := a.cfg.Realtime
	a.saveConfig()
	a.cfgMu.Unlock()

	// 配置变了：结束进行中的会话（旧后端），再热重建。stopRealtimeSession 自己管 rtMu，故先调它、
	// 不能在持有 rtMu 时调（会死锁）。
	a.stopRealtimeSession()
	a.rtMu.Lock()
	a.rtBackend = buildRealtimeBackend(rtCfg)
	a.rtMu.Unlock()
	a.log.Info("实时语音配置已更新", "enabled", rtCfg.Enabled, "provider", rtCfg.Provider, "model", rtCfg.Model)
	a.srv.Broadcast(a.statusSnapshot())
}

// handleSetDevice 设置设备角色(人设)与音色：写配置落盘；人设变化即时更新系统提示。
func (a *app) handleSetDevice(cmd protocol.SetDeviceCommand) {
	a.cfgMu.Lock()
	a.cfg.Persona = cmd.Persona
	a.cfg.Voice = cmd.Voice
	if cmd.PersonaSource == "local" || cmd.PersonaSource == "model" {
		a.cfg.PersonaSource = cmd.PersonaSource
	}
	persona := a.cfg.Persona
	a.saveConfig()
	a.cfgMu.Unlock()
	// 即时生效：替换对话历史里的系统提示（人设 + 固定工具铁律）。
	a.histMu.Lock()
	if len(a.history) > 0 && a.history[0].Role == llm.RoleSystem {
		a.history[0].Content = buildSystemPrompt(persona)
	}
	a.histMu.Unlock()
	a.log.Info("更新设备角色与音色", "voice", cmd.Voice)
	a.srv.Broadcast(a.statusSnapshot())
}

// handleSetVolume 设置设备扬声器音量(1~100)并落盘，立即生效（设置页滑块）。
func (a *app) handleSetVolume(cmd protocol.SetVolumeCommand) {
	v := cmd.Volume
	if v < 1 {
		v = 1
	} else if v > 100 {
		v = 100
	}
	a.cfgMu.Lock()
	a.cfg.IO.DeviceVolume = v
	device := a.cfg.IO.AudioDevice
	a.saveConfig()
	a.cfgMu.Unlock()
	a.applyDeviceVolume(device, v)
	a.log.Info("设置设备音量", "vol", v)
	a.srv.Broadcast(a.statusSnapshot())
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

// reportLLMErr 把大模型错误报给页面——但【我们自己取消的那次请求不算错误】。
//
// 一次说话被 ASR 切成几段、或者用户接着又说了一句，handleChat 就会 turnCancel 掉上一轮（这是有意
// 设计的：保证一次只处理一轮、回复不乱序）。被取消的 HTTP 请求会返回 "context canceled"，以前原样
// 弹成红色错误条，看起来就像"大模型经常超时/失败"，其实什么事都没有。
func reportLLMErr(a *app, ctx context.Context, code string, err error) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		a.log.Debug("对话已被新一轮打断（非错误）", "err", err)
		return
	}
	a.log.Warn("大模型调用失败", "code", code, "err", err)
	a.srv.Broadcast(protocol.ErrorEvent{Code: code, Message: err.Error()})
}
