package main

// MiniMax 多模态 I/O 路由：把"文字→语音""文字→图片"的产物，按 config.io 的开关
// 路由到设备(主)与/或页面(调试镜像)。
//   - 语音：tts_engine=minimax 时 host 合成 mp3 → audio_out 决定 设备(mpg123)/页面(浏览器)。
//   - 图片：generate_image 工具产图 → image_out 决定 设备屏(ShowImage)/页面(聊天配图)。

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kingGang/ElectronStudio/internal/config"
	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/minimax"
	"github.com/kingGang/ElectronStudio/internal/music"
	"github.com/kingGang/ElectronStudio/internal/protocol"
	"github.com/kingGang/ElectronStudio/internal/speech"
)

// thinkRe 匹配推理模型（如 MiniMax-M3）输出在正文里的 <think>…</think> 思考块。
var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThink 去掉回复正文里的 <think> 推理块（含未闭合的残块），用于显示与 TTS 前清洗，
// 避免机器人把自己的思考过程显示/念出来。
func stripThink(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "<think>"); i >= 0 { // 未闭合：丢弃从 <think> 到结尾
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ---- 生成图片的内存暂存：供页面通过 HTTP 取回展示（保留最近 N 张）----

const maxGenImages = 24

type genImgStore struct {
	mu    sync.Mutex
	m     map[string][]byte
	order []string
	seq   int
}

func newGenImgStore() *genImgStore { return &genImgStore{m: make(map[string][]byte)} }

func (s *genImgStore) put(data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := strconv.Itoa(s.seq)
	s.m[id] = data
	s.order = append(s.order, id)
	for len(s.order) > maxGenImages {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
	return id
}

func (s *genImgStore) get(id string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id]
}

// handleGenImg 返回某张生成图（GET /api/genimg?id=）。
func (a *app) handleGenImg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET", http.StatusMethodNotAllowed)
		return
	}
	data := a.genimg.get(r.URL.Query().Get("id"))
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// ---- 语音输出路由 ----

// ioSnap 是 io 配置的一次性快照（在 cfgMu 下读取，避免与设置页改模型并发竞态）。
type ioSnap struct {
	AudioIn   string
	AudioOut  string
	TTSEngine string
	ImageOut  string
	Voice     string // 设备音色（覆盖当前引擎音色），来自 config.voice
	MM        config.MiniMaxConfig
}

// ioSnapshot 在 cfgMu 下取一份 io 配置快照（含解析后的 MiniMax 凭据）。
func (a *app) ioSnapshot() ioSnap {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	io := a.cfg.IO
	return ioSnap{
		AudioIn:   io.AudioInOr(),
		AudioOut:  io.AudioOutOr(),
		TTSEngine: io.TTSEngineOr(),
		ImageOut:  io.ImageOutOr(),
		Voice:     a.cfg.Voice,
		MM:        a.cfg.ResolveMiniMax(),
	}
}

// setSpeakCancel 记录本段语音的取消函数，并取消上一段（同一时刻只允许一段在说）。
func (a *app) setSpeakCancel(c context.CancelFunc) {
	a.speakMu.Lock()
	if a.speakCancel != nil {
		a.speakCancel()
	}
	a.speakCancel = c
	a.speakMu.Unlock()
}

// cancelSpeak 取消进行中的语音（合成/播放），用于打断（barge-in）。
func (a *app) cancelSpeak() {
	a.speakMu.Lock()
	if a.speakCancel != nil {
		a.speakCancel()
		a.speakCancel = nil
	}
	a.speakMu.Unlock()
}

// speak 把文本转语音并按配置路由播放（替代直接 a.speech.Speak）。
// tts_engine=minimax 时 host 合成 mp3 → routeAudio；否则回退 sidecar（在设备侧合成播放）。
// 全程可被 interrupt 取消（barge-in）：合成中取消则不播放，播放中取消则杀 mpg123。
func (a *app) speak(parent context.Context, text string) {
	if text == "" {
		return
	}
	io := a.ioSnapshot()
	if io.AudioOut == "off" { // 别出声：不合成、不播放
		return
	}
	// tts_engine=xiaozhi：用当前模型（小智）自带 TTS。音频在本轮已由 streamXiaozhiAudio
	// 逐句流式播放（边收边播），这里无需再处理。
	if io.TTSEngine == "xiaozhi" {
		if a.audioStreamed.Load() {
			return // 已流式播放完毕
		}
		// 本轮没有流式音频：可能是非流式路径(Complete)留下的整段，或当前模型不是小智。
		if ogg := a.llm.ActiveAudioOgg(); len(ogg) > 0 {
			a.log.Info("使用小智自带语音(整段)", "bytes", len(ogg), "out", io.AudioOut)
			sctx, cancel := context.WithCancel(context.Background())
			a.setSpeakCancel(cancel)
			a.routeOgg(sctx, io.AudioOut, ogg, text)
			return
		}
		// 没有小智音频（当前模型不是小智 / 未回传）→ 回退 sidecar 合成。
		a.log.Warn("tts_engine=xiaozhi 但本轮无小智音频，回退 sidecar（确认当前模型为小智且设备已在 xiaozhi.me 激活）")
		if err := a.speech.Speak(parent, text); err != nil {
			a.log.Warn("sidecar 语音失败", "err", err)
		}
		return
	}
	if io.TTSEngine == "openai" && a.netTTS != nil {
		sctx, cancel := context.WithCancel(context.Background())
		a.setSpeakCancel(cancel)
		audio, err := a.netTTS.Synthesize(sctx, text, io.Voice)
		if sctx.Err() != nil {
			return
		}
		if err != nil {
			a.log.Warn("网络 TTS 合成失败，回退 sidecar", "err", err)
			if e := a.speech.Speak(parent, text); e != nil {
				a.log.Warn("sidecar 语音失败", "err", e)
			}
			return
		}
		a.routeAudio(sctx, io.AudioOut, audio, text)
		return
	}
	if io.TTSEngine == "minimax" && a.mm != nil {
		sctx, cancel := context.WithCancel(context.Background())
		a.setSpeakCancel(cancel)
		voiceID := io.MM.VoiceID
		if io.Voice != "" {
			voiceID = io.Voice // 设备音色覆盖
		}
		mp3, err := a.mm.Synthesize(sctx, text, minimax.SpeakOptions{Model: io.MM.TTSModel, VoiceID: voiceID})
		if sctx.Err() != nil { // 合成期间被打断，直接放弃
			return
		}
		if err != nil {
			a.log.Warn("MiniMax 语音合成失败，回退 sidecar", "err", err)
			if e := a.speech.Speak(parent, text); e != nil {
				a.log.Warn("sidecar 语音失败", "err", e)
			}
			return
		}
		a.routeAudio(sctx, io.AudioOut, mp3, text)
		return
	}
	if err := a.speech.Speak(parent, text); err != nil {
		a.log.Warn("sidecar 语音失败", "err", err)
	}
}

// routeAudio 把一段 mp3 按 audio_out 路由到设备(mpg123)与/或页面(base64 推浏览器)。
// ctx 为本段语音的可取消上下文：interrupt 取消它会杀掉设备播放（barge-in）。
func (a *app) routeAudio(ctx context.Context, out string, mp3 []byte, text string) {
	if out == "device" || out == "both" {
		go func() {
			if err := a.player.Play(ctx, mp3); err != nil && ctx.Err() == nil {
				a.log.Warn("设备播放音频失败（确认已装 mpg123）", "err", err)
			}
		}()
	}
	if out == "page" || out == "both" {
		a.srv.Broadcast(protocol.AudioEvent{
			Format: "mp3", Data: base64.StdEncoding.EncodeToString(mp3), Text: text,
		})
	}
}

// streamXiaozhiAudio 是小智流式音频的回调：每收到一句的 Ogg/Opus 就即时播放（边收边播）。
// 页面端按到达顺序排队顺序播放；设备端交 sidecar 串行解码播放。第一句到达时即标记本轮已出声，
// 收尾的 speak 据此不再重复合成。
func (a *app) streamXiaozhiAudio(ogg []byte) {
	if len(ogg) == 0 {
		return
	}
	io := a.ioSnapshot()
	if io.AudioOut == "off" {
		return
	}
	// 仅当 tts_engine=xiaozhi 时才用小智自带嗓音；否则忽略小智回传的音频，
	// 交由 speak() 用所选引擎（minimax/openai）合成，避免两路声音叠加（回音）。
	if io.TTSEngine != "xiaozhi" {
		return
	}
	first := a.audioStreamed.CompareAndSwap(false, true)
	if first {
		a.log.Info("小智流式语音开始", "out", io.AudioOut)
	}
	if io.AudioOut == "page" || io.AudioOut == "both" {
		a.srv.Broadcast(protocol.AudioEvent{
			Format: "ogg", Data: base64.StdEncoding.EncodeToString(ogg), Stream: true,
		})
	}
	if io.AudioOut == "device" || io.AudioOut == "both" {
		if sc, ok := a.speech.(*speech.Sidecar); ok {
			if err := sc.PlayAudio(context.Background(), "ogg", ogg); err != nil {
				a.log.Warn("设备播放小智流式音频失败", "err", err)
			}
		}
	}
}

// routeOgg 把一段 Ogg/Opus（小智自带 TTS）按 audio_out 路由：
// 页面直接播 audio/ogg（浏览器原生解码）；设备交语音 sidecar 解码后用 sounddevice 播放
// （主程序无 cgo，故不在 Go 里解码，也不依赖 ffmpeg）。
func (a *app) routeOgg(ctx context.Context, out string, ogg []byte, text string) {
	if out == "page" || out == "both" {
		a.srv.Broadcast(protocol.AudioEvent{
			Format: "ogg", Data: base64.StdEncoding.EncodeToString(ogg), Text: text,
		})
	}
	if out == "device" || out == "both" {
		sc, ok := a.speech.(*speech.Sidecar)
		if !ok {
			a.log.Warn("设备播放小智音频需要语音 sidecar，当前未启用")
			return
		}
		go func() {
			if err := sc.PlayAudio(ctx, "ogg", ogg); err != nil && ctx.Err() == nil {
				a.log.Warn("设备播放小智音频失败（确认 sidecar 已连接）", "err", err)
			}
		}()
	}
}

// ---- 图片输出路由 ----

// handleGenerateImage 是 generate_image 工具的执行体：生成图片并按 image_out 路由。
func (a *app) handleGenerateImage(ctx context.Context, prompt string) (string, error) {
	if a.mm == nil {
		return "", fmt.Errorf("未配置 MiniMax，无法生成图片")
	}
	io := a.ioSnapshot()
	if io.ImageOut == "off" {
		return "图片输出已关闭（image_out=off）", nil
	}
	img, err := a.mm.GenerateImage(ctx, prompt, minimax.ImageOptions{Model: io.MM.ImageModel, AspectRatio: "1:1"})
	if err != nil {
		return "", err
	}
	if io.ImageOut == "device" || io.ImageOut == "both" {
		if rgb, err := display.DecodeToScreen(img); err == nil {
			a.screen.ShowImage(rgb, 12) // 设备屏显示 ~12 秒
		} else {
			a.log.Warn("生成图转屏幕失败", "err", err)
		}
	}
	if io.ImageOut == "page" || io.ImageOut == "both" {
		id := a.genimg.put(img)
		a.srv.Broadcast(protocol.ChatEvent{
			ID: a.nextMsgID(), Role: protocol.RoleAssistant,
			Images: []string{"/api/genimg?id=" + id}, Status: protocol.ChatFinal,
		})
	}
	return "已生成并显示图片：" + prompt, nil
}

// handleGenerateMusic 是 generate_music 工具的执行体：用 MiniMax 生成音乐并按 audio_out 路由播放。
// 设备侧经 mpg123（可被打断）；页面侧存储后以 URL 推给浏览器播放（音乐较大，不走 base64）。
func (a *app) handleGenerateMusic(ctx context.Context, prompt, lyrics string) (string, error) {
	if a.mm == nil {
		return "", fmt.Errorf("未配置 MiniMax，无法生成音乐")
	}
	io := a.ioSnapshot()
	if io.AudioOut == "off" {
		return "音频输出已关闭（audio_out=off）", nil
	}
	mp3, err := a.mm.GenerateMusic(ctx, prompt, minimax.MusicOptions{Model: io.MM.MusicModel, Lyrics: lyrics})
	if err != nil {
		return "", err
	}
	if io.AudioOut == "device" || io.AudioOut == "both" {
		sctx, cancel := context.WithCancel(context.Background())
		a.setSpeakCancel(cancel) // 复用"当前设备音频"取消槽，interrupt 可停
		go func() {
			if e := a.player.Play(sctx, mp3); e != nil && sctx.Err() == nil {
				a.log.Warn("设备播放音乐失败（确认已装 mpg123）", "err", e)
			}
		}()
	}
	if io.AudioOut == "page" || io.AudioOut == "both" {
		// 作为聊天里的播放器卡片展示（可见、可重播），而非后台静默播放。
		id := a.genaudio.put(mp3)
		a.srv.Broadcast(protocol.ChatEvent{
			ID: a.nextMsgID(), Role: protocol.RoleAssistant,
			Content: "♪ " + prompt, Audio: "/api/genaudio?id=" + id, Status: protocol.ChatFinal,
		})
	}
	return "已生成音乐：" + prompt, nil
}

// handleGenAudio 通过 HTTP 返回某段生成音频（GET /api/genaudio?id=），供页面播放较大的音乐。
func (a *app) handleGenAudio(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET", http.StatusMethodNotAllowed)
		return
	}
	data := a.genaudio.get(r.URL.Query().Get("id"))
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleQQLoginStart 开始一次 QQ 音乐扫码登录，返回二维码 PNG。
func (a *app) handleQQLoginStart(w http.ResponseWriter, r *http.Request) {
	q := music.NewQRLogin()
	png, err := q.Start(r.Context())
	if err != nil {
		http.Error(w, "出码失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	a.qrMu.Lock()
	a.qrLogin = q
	a.qrMu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// handleQQLoginPoll 轮询扫码状态；成功则写入 cookie、持久化并热替换音源。
func (a *app) handleQQLoginPoll(w http.ResponseWriter, r *http.Request) {
	a.qrMu.Lock()
	q := a.qrLogin
	a.qrMu.Unlock()
	if q == nil {
		writeJSON(w, map[string]string{"state": "error", "message": "请先获取二维码"})
		return
	}
	res, _ := q.Poll(r.Context())
	if res.State == "ok" && res.Cookie != "" {
		a.cfgMu.Lock()
		a.cfg.Music.Source = "qq"
		a.cfg.Music.QQ = config.QQConfig{Cookie: res.Cookie}
		a.saveConfig()
		a.cfgMu.Unlock()
		a.music.SetSearcher(music.NewQQMusicSearcher(res.Cookie, "", ""))
		a.log.Info("QQ 音乐扫码登录成功，已更新 cookie 并切换音源")
		a.srv.Broadcast(a.statusSnapshot())
	}
	writeJSON(w, map[string]string{"state": res.State, "message": res.Message})
}

// handleTranscribe 网络 ASR：接收页面上传的音频，转发给配置的 OpenAI 兼容识别服务，返回文字。
func (a *app) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if a.netASR == nil {
		http.Error(w, "未配置网络 ASR（speech.asr）", http.StatusServiceUnavailable)
		return
	}
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20)) // 上限 25MB
	if err != nil || len(audio) == 0 {
		http.Error(w, "读取音频失败", http.StatusBadRequest)
		return
	}
	name := r.URL.Query().Get("name")
	text, err := a.netASR.Transcribe(r.Context(), audio, name)
	if err != nil {
		a.log.Warn("网络 ASR 识别失败", "err", err)
		http.Error(w, "识别失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"text": text})
}

// openAIVoices 是 OpenAI 兼容 TTS 的内置音色（无列表 API，用已知集合）。
var openAIVoices = []minimax.Voice{
	{ID: "alloy", Name: "Alloy"}, {ID: "echo", Name: "Echo"}, {ID: "fable", Name: "Fable"},
	{ID: "onyx", Name: "Onyx"}, {ID: "nova", Name: "Nova"}, {ID: "shimmer", Name: "Shimmer"},
	{ID: "ash", Name: "Ash"}, {ID: "ballad", Name: "Ballad"}, {ID: "coral", Name: "Coral"},
	{ID: "sage", Name: "Sage"}, {ID: "verse", Name: "Verse"},
}

const voiceSampleText = "你好，我是你的桌面机器人小电，很高兴为你服务～"

// handleVoicePreview 用指定音色合成一句样例音频返回，供设置页试听。
func (a *app) handleVoicePreview(w http.ResponseWriter, r *http.Request) {
	voice := r.URL.Query().Get("voice")
	io := a.ioSnapshot()
	var audio []byte
	var err error
	switch io.TTSEngine {
	case "openai":
		if a.netTTS == nil {
			http.Error(w, "未配置网络 TTS", http.StatusServiceUnavailable)
			return
		}
		audio, err = a.netTTS.Synthesize(r.Context(), voiceSampleText, voice)
	default: // minimax
		if a.mm == nil {
			http.Error(w, "未配置 MiniMax", http.StatusServiceUnavailable)
			return
		}
		v := voice
		if v == "" {
			v = io.MM.VoiceID
		}
		audio, err = a.mm.Synthesize(r.Context(), voiceSampleText, minimax.SpeakOptions{Model: io.MM.TTSModel, VoiceID: v})
	}
	if err != nil {
		a.log.Warn("音色试听合成失败", "voice", voice, "err", err)
		http.Error(w, "试听失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(audio)
}

// cloneVoiceIDRe 仅保留字母数字，用于从用户名字构造合法 voice_id。
var cloneVoiceIDRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

// handleVoiceClone 从上传的音频克隆出一个 MiniMax 音色，返回新 voice_id。
func (a *app) handleVoiceClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if a.mm == nil {
		http.Error(w, "未配置 MiniMax（克隆音色需要 MiniMax）", http.StatusServiceUnavailable)
		return
	}
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil || len(audio) == 0 {
		http.Error(w, "读取音频失败", http.StatusBadRequest)
		return
	}
	// 构造合法 voice_id：字母开头、含字母与数字、≥8 位。
	base := cloneVoiceIDRe.ReplaceAllString(r.URL.Query().Get("name"), "")
	if base == "" || !((base[0] >= 'a' && base[0] <= 'z') || (base[0] >= 'A' && base[0] <= 'Z')) {
		base = "voice" + base
	}
	voiceID := base + strconv.FormatInt(time.Now().Unix(), 10) // 追加数字保证含数字且唯一
	name := r.URL.Query().Get("file")
	fileID, err := a.mm.UploadFile(r.Context(), audio, name, "voice_clone")
	if err != nil {
		a.log.Warn("克隆-上传失败", "err", err)
		http.Error(w, "上传失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := a.mm.CloneVoice(r.Context(), fileID, voiceID); err != nil {
		a.log.Warn("克隆失败", "err", err)
		http.Error(w, "克隆失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	a.log.Info("音色克隆成功", "voice_id", voiceID)
	writeJSON(w, map[string]string{"voice_id": voiceID})
}

// handleVoices 返回当前 TTS 引擎的可用音色列表，供设置页下拉选择。
// minimax: 调 get_voice 动态拉取(含克隆音色)；openai: 内置集合；sidecar: 由模型决定，返回空。
func (a *app) handleVoices(w http.ResponseWriter, r *http.Request) {
	engine := a.ioSnapshot().TTSEngine
	type voiceList struct {
		Engine string          `json:"engine"`
		Voices []minimax.Voice `json:"voices"`
	}
	out := voiceList{Engine: engine, Voices: []minimax.Voice{}}
	switch engine {
	case "minimax":
		if a.mm != nil {
			if vs, err := a.mm.ListVoices(r.Context()); err != nil {
				a.log.Warn("拉取 MiniMax 音色失败", "err", err)
			} else {
				out.Voices = vs
			}
		}
	case "openai":
		out.Voices = openAIVoices
	}
	writeJSON(w, out)
}

// allowedMusicHost 限定可代理的音乐 CDN 域名，避免成为开放代理（SSRF）。
func allowedMusicHost(h string) bool {
	h = strings.ToLower(h)
	return strings.HasSuffix(h, ".qq.com") || strings.Contains(h, "qqmusic") ||
		strings.Contains(h, "kuwo") || strings.Contains(h, "music.126.net")
}

// handleMusicProxy 把在线音乐流以同源方式转发给页面：跨域音频在浏览器 Web Audio 的
// AnalyserNode 里只会拿到全 0（无法画波形），经本服务代理后变为同源即可读到采样。
// 转发 Range 头以支持页面拖动进度。
func (a *app) handleMusicProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "缺少 url", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !allowedMusicHost(u.Hostname()) {
		http.Error(w, "不允许的地址", http.StatusForbidden)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://y.qq.com/")
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "上游取流失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
