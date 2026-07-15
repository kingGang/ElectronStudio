package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/kingGang/ElectronStudio/internal/config"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/realtime"
	"github.com/kingGang/ElectronStudio/internal/speech"
)

// 唤醒/退出提示语，用 Qwen 音色预先录好（与实时对话音色一致，不用假的本地 TTS）。
// 裸 PCM：24kHz / 单声道 / int16 小端。录制脚本见 scratchpad/record_prompts.py。
//
//go:embed assets/greeting.pcm
var greetingPCM []byte

//go:embed assets/goodbye.pcm
var goodbyePCM []byte

// promptSecs 估算一段 24k/单声道/int16 PCM 的播放秒数（用于播完再开麦/关会话的定时）。
func promptSecs(pcm []byte) time.Duration {
	return time.Duration(float64(len(pcm))/2/24000*float64(time.Second)) + 300*time.Millisecond
}

// realtimeSession 是一次进行中的实时语音会话。生命周期：唤醒词命中 → start，静默超时 /
// 显式结束 → stop。会话内麦克风原始音频经 sidecar 转发上云，云端回音直播到设备喇叭。
type realtimeSession struct {
	client   *realtime.Client
	cancel   context.CancelFunc
	idle     *time.Timer // 静默计时：超时自动结束会话（省流量与费用）
	lastUser string      // 用户上一句（助手回复中性时用它共情定表情）
}

// emotionKeywords 是情绪关键词表（按优先级，命中多者胜、并列取靠前）。用于按对话内容自动定表情。
var emotionKeywords = []struct {
	emo string
	kws []string
}{
	{"happy", []string{"哈哈", "嘿嘿", "嘻嘻", "开心", "太好了", "好耶", "真好", "不错", "棒", "喜欢", "高兴", "开森", "赞", "耶", "好呀", "好的呀", "😄", "😊", "🎉"}},
	{"surprised", []string{"哇", "哇塞", "天哪", "我的天", "居然", "竟然", "不会吧", "真的假的", "太厉害", "没想到", "厉害了", "wow", "😮", "😲"}},
	{"sad", []string{"难过", "伤心", "抱歉", "对不起", "遗憾", "可惜", "呜呜", "失望", "难受", "委屈", "心疼", "😢", "😭"}},
	{"angry", []string{"生气", "讨厌", "气死", "可恶", "烦死", "岂有此理", "愤怒", "气人", "过分", "😠", "😡"}},
	{"confused", []string{"奇怪", "不懂", "为什么", "怎么会", "搞不懂", "疑惑", "不明白", "啥意思", "🤔"}},
	{"silly", []string{"鬼脸", "做个鬼脸", "扮鬼脸", "调皮", "搞怪", "吐舌", "皮一下", "逗你", "😜", "😝", "🤪"}},
}

// sentimentEmotion 按关键词粗略判断文本情绪，返回 6 情绪之一或 "neutral"。给实时对话自动定表情用。
func sentimentEmotion(text string) string {
	if text == "" {
		return "neutral"
	}
	bestEmo, bestN := "neutral", 0
	for _, g := range emotionKeywords {
		n := 0
		for _, k := range g.kws {
			n += strings.Count(text, k)
		}
		if n > bestN {
			bestN, bestEmo = n, g.emo
		}
	}
	return bestEmo
}

// setFaceEmotion 只改【表情脸】+ 广播情绪(供 UI)，【不】触发同名动作——实时对话按语义频繁变表情，
// 不该每句都让机器人做一次肢体动作(既吵又费舵机)。
func (a *app) setFaceEmotion(e string) {
	a.screen.SetEmotion(e)
	a.srv.Broadcast(protocol.EmotionEvent{Emotion: protocol.Emotion(e)})
}

// realtimeIdleTimeout：这么久没有新的用户音频/回复，就结束会话（下次唤醒再建）。
const realtimeIdleTimeout = 30 * time.Second

// buildRealtimeBackend 按配置构造实时后端；未启用或缺 key 返回 nil。
func buildRealtimeBackend(cfg config.RealtimeConfig) realtime.Backend {
	if !cfg.Enabled || cfg.APIKey == "" {
		return nil
	}
	switch cfg.Provider {
	case "", "qwen":
		return &realtime.QwenBackend{WSBase: cfg.WSBase, Model: cfg.Model, APIKey: cfg.APIKey, Voice: cfg.Voice}
	// case "glm": 待实现 GLMBackend
	default:
		return nil
	}
}

// realtimeEnabled 报告实时语音是否可用（配置开启且后端已就绪）。
// 用 rtMu 保护：设置页 handleSetRealtime 会热替换 rtBackend，与此并发读冲突。
func (a *app) realtimeEnabled() bool {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	return a.rtBackend != nil
}

// toolDefs 把当前工具注册表转换为 realtime 的中立工具定义（字段一一对应）。
func (a *app) toolDefs() []realtime.ToolDef {
	specs := a.tools.Specs()
	out := make([]realtime.ToolDef, 0, len(specs))
	for _, s := range specs {
		out = append(out, realtime.ToolDef{Name: s.Name, Description: s.Description, Parameters: s.Parameters})
	}
	return out
}

// startRealtimeSession 在唤醒时建立会话：连云端、开麦克风原始音频上行、起事件消费循环。
// 已在会话中则只续期静默计时。
func (a *app) startRealtimeSession(parent context.Context) {
	a.rtMu.Lock()
	if a.rtSession != nil {
		a.rtSession.idle.Reset(realtimeIdleTimeout)
		a.rtMu.Unlock()
		return
	}
	backend := a.rtBackend // 同锁内取，避免与 handleSetRealtime 的热替换竞争
	a.rtMu.Unlock()
	if backend == nil {
		return // 会话建立前配置刚被关掉/热重建成 nil
	}

	client := realtime.New(backend, a.log)
	ctx, cancel := context.WithCancel(parent)
	persona := buildSystemPrompt(a.cfg.Persona)
	if err := client.Connect(ctx, persona, a.toolDefs()); err != nil {
		a.log.Warn("实时会话建立失败", "err", err)
		cancel()
		return
	}
	sess := &realtimeSession{client: client, cancel: cancel}
	sess.idle = time.AfterFunc(realtimeIdleTimeout, func() {
		a.log.Info("实时会话静默超时，结束")
		a.playGoodbyeAndStop() // 超时退出也播告别（退出必有反馈）
	})
	a.rtMu.Lock()
	a.rtSession = sess
	a.rtMu.Unlock()

	a.log.Info("实时语音会话已开始")
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceListening})
	go a.consumeRealtime(ctx, sess)

	// 唤醒反馈 + 延迟开麦：播预录的 Qwen 音色问候语（与对话音色一致），估时等它播完再【首次】开麦。
	//
	// 【为什么不让云端实时说问候】：试过会话刚建立就 SendText 让云端打招呼——那一瞬间 session 还没
	// 就绪，云端只建 item、不生成 response；而首次开麦曾绑在问候的 response.done 上，于是问候没
	// response → 麦永远不开 → 唤醒后毫无反应（踩过这个坑）。故首次开麦【不依赖云端】：播固定音频 +
	// 固定延迟这条可靠路径。音色一致靠预录（见 greetingPCM）。
	go func() {
		sc, ok := a.speech.(*speech.Sidecar)
		if !ok {
			return
		}
		pctx, pc := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sc.PlayPCM(pctx, greetingPCM, 24000)
		pc()
		time.Sleep(promptSecs(greetingPCM)) // 等问候播完再开麦，免得机器人把自己的问候当用户输入
		a.rtMu.Lock()
		still := a.rtSession == sess // 期间可能已被退出/超时结束
		a.rtMu.Unlock()
		if !still {
			return
		}
		sctx, scc := context.WithTimeout(context.Background(), 3*time.Second)
		_ = sc.StreamStart(sctx)
		scc()
	}()
}

// playGoodbyeAndStop 播告别语后结束会话——所有"用户可感知的退出"(语音退出命令 / 静默超时)都走它，
// 保证退出一定有语音反馈。先 abort 掉上一轮还在播的音频，让告别能立刻播；估时播完再真正关会话。
func (a *app) playGoodbyeAndStop() {
	if sc, ok := a.speech.(*speech.Sidecar); ok {
		sc.Stop() // 停掉上一轮尚未播完的音频，让告别立刻插播
		pctx, pc := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sc.PlayPCM(pctx, goodbyePCM, 24000)
		pc()
		time.Sleep(promptSecs(goodbyePCM)) // 等告别播完再关
	}
	a.stopRealtimeSession()
}

// stopRealtimeSession 结束当前会话：停麦克风上行、关云端连接。幂等。
func (a *app) stopRealtimeSession() {
	a.rtMu.Lock()
	sess := a.rtSession
	a.rtSession = nil
	a.rtMu.Unlock()
	if sess == nil {
		return
	}
	sess.idle.Stop()
	// 注意【不在这里 abort】：退出时若还有告别语在播，abort 会把它截断。上一轮残留音频的 abort
	// 由退出分支自己在 SendText 告别【之前】做（见 consumeRealtime）。这里只停上行 + 关连接。
	if sc, ok := a.speech.(*speech.Sidecar); ok {
		ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		_ = sc.StreamStop(ctx)
		c()
	}
	sess.cancel()
	_ = sess.client.Close()
	a.screen.SetSpeaking(false)    // 会话结束，嘴合上、停口型
	a.screen.SetEmotion("neutral") // 回到中性表情，别停在最后一句的情绪上
	a.log.Info("实时语音会话已结束")
	a.srv.Broadcast(protocol.VoiceStateEvent{State: protocol.VoiceIdle})
}

// feedRealtimeAudio 把一块麦克风原始音频推给云端（KindAudio 事件调用）。会话未开则忽略。
func (a *app) feedRealtimeAudio(pcm []byte) {
	a.rtMu.Lock()
	sess := a.rtSession
	a.rtMu.Unlock()
	if sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.client.PushAudio(ctx, pcm); err != nil {
		a.log.Debug("推送实时音频失败", "err", err)
	}
}

// consumeRealtime 消费云端下行事件：音频→设备喇叭、转写→UI、函数调用→本地执行、打断→停播放。
func (a *app) consumeRealtime(ctx context.Context, sess *realtimeSession) {
	sc, _ := a.speech.(*speech.Sidecar)
	// 本轮回复的下行音频统计：云端把整段音频一股脑 burst 下来、response.done 时远没播完，
	// 据总字节估算真实播放时长，让口型/半双工麦克风持续到音频真正播完。
	var audioBytes int
	var firstAudio time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sess.client.Events():
			if !ok {
				return
			}
			sess.idle.Reset(realtimeIdleTimeout) // 有活动就续期
			switch ev.Kind {
			case realtime.KindResponseStarted:
				// 机器人开始说话 → 【半双工】暂停上行麦克风。否则设备麦离喇叭近，机器人会听到
				// 自己的回音 → 云端 VAD 不停触发 → 自我打断/无限对话（实测过，喇叭永远出不了声）。
				if sc != nil {
					sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
					_ = sc.StreamStop(sctx)
					c()
				}
				audioBytes = 0
				firstAudio = time.Time{}
				a.screen.SetSpeaking(true) // 驱动表情口型，与对话同步
			case realtime.KindAudio:
				// 累计音频字节 + 记首块时刻，用于估算真实播放时长(见 KindResponseDone)。
				// 注意：云端把整段音频【一股脑 burst 下来】，不是实时节奏，所以【不】按到达节奏算 RMS
				// 驱动口型(会快得离谱、且提前放完)——改由说话态的固定节奏张合，覆盖整段播放时长。
				if firstAudio.IsZero() {
					firstAudio = time.Now()
				}
				audioBytes += len(ev.Audio)
				if sc != nil {
					pctx, c := context.WithTimeout(context.Background(), 5*time.Second)
					if err := sc.PlayPCM(pctx, ev.Audio, 24000); err != nil { // 云端下行 pcm 24k
						a.log.Warn("实时音频送设备失败", "err", err)
					}
					c()
				}
			case realtime.KindAssistantText:
				a.srv.Broadcast(protocol.ChatEvent{Role: "assistant", Content: ev.Text, Status: "done"})
				// 按语义自动定表情：机器人说的话是啥情绪，脸就是啥；它中性时，共情用户上一句。
				emo := sentimentEmotion(ev.Text)
				if emo == "neutral" {
					emo = sentimentEmotion(sess.lastUser)
				}
				a.setFaceEmotion(emo)
			case realtime.KindUserTranscript:
				sess.lastUser = ev.Text
				a.srv.Broadcast(protocol.ASREvent{Text: ev.Text, Final: true})
				// 语音退出：说"退出/退下/再见"等 → 播预录的 Qwen 音色告别语，播完关会话。
				// 先 abort 掉上一轮还在播的音频（否则退出后它还会把上句讲完），再播告别（abort 之后入队，
				// 不会被清），估时播完再关。告别音频与对话音色一致（见 goodbyePCM）。
				if isExitCommand(ev.Text) {
					a.log.Info("收到语音退出命令，播报告别后结束", "text", ev.Text)
					go a.playGoodbyeAndStop() // 播告别再关（退出必有反馈）
					return                    // 会话即将关闭，退出消费循环
				}
			case realtime.KindSpeechStarted:
				// 半双工下机器人说话时上行已停，故收到它=用户正常开口，不需要打断动作。
			case realtime.KindFunctionCall:
				go a.runRealtimeTool(ctx, sess, ev)
			case realtime.KindResponseDone:
				// response.done 只表示云端【发完】音频，sidecar 还在按真实时长播——据总字节估播放时长，
				// 让口型和半双工麦克风都【等音频真正播完】再收：否则嘴提前停(后面不动)、麦提前开(回音)。
				playDur := time.Duration(audioBytes) * time.Second / (2 * 24000) // 24kHz mono int16
				remain := playDur - time.Since(firstAudio)
				go func() {
					if remain > 0 {
						time.Sleep(remain)
					}
					a.screen.SetSpeaking(false) // 音频真播完了，嘴合上
					if sc != nil {
						sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
						_ = sc.StreamStart(sctx) // 播完再恢复麦克风，避免机器人听到自己的尾音
						c()
					}
				}()
			case realtime.KindError:
				a.log.Warn("实时会话错误", "msg", ev.Text)
			}
		}
	}
}

// pcmMouthLevel 计算一块 PCM16 音频的 RMS 并映射到口型开合 0..1，让嘴按说话响度张合。
func pcmMouthLevel(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		f := float64(s) / 32768.0
		sum += f * f
	}
	rms := math.Sqrt(sum / float64(n))
	return math.Min(rms*4.5, 1) // 语音 RMS 常在 0.05~0.25，增益后铺满 0..1
}

// runRealtimeTool 执行一次云端请求的工具调用并把结果回传，触发云端基于结果续答。
func (a *app) runRealtimeTool(ctx context.Context, sess *realtimeSession, ev realtime.Event) {
	result, err := a.tools.Execute(ctx, ev.FuncName, ev.FuncArgs)
	if err != nil {
		a.log.Warn("实时工具执行失败", "name", ev.FuncName, "err", err)
		result = toolErrJSON(err) // 把错误也回传给模型，让它自然告知用户
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sess.client.SendFunctionResult(rctx, ev.CallID, result); err != nil {
		a.log.Warn("回传工具结果失败", "err", err)
	}
}

// toolErrJSON 把工具错误包装成一段 JSON 结果字符串回给模型。
func toolErrJSON(err error) string {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(b)
}

// exitPhrases 是主动结束实时会话的语音命令关键词。用【包含】匹配，故写得明确些、避免误伤
// （如不用单字"关闭"，否则"关闭台灯"会误退出）。
var exitPhrases = []string{"退出", "再见", "拜拜", "结束对话", "结束会话", "关闭对话", "退下", "不聊了", "先这样", "下线"}

// isExitCommand 判断一句用户转写是否为"退出实时对话"命令。
func isExitCommand(text string) bool {
	t := strings.Trim(strings.TrimSpace(text), "，。！？,.!?、 ")
	for _, w := range exitPhrases {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}
