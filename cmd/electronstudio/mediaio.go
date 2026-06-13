package main

// MiniMax 多模态 I/O 路由：把"文字→语音""文字→图片"的产物，按 config.io 的开关
// 路由到设备(主)与/或页面(调试镜像)。
//   - 语音：tts_engine=minimax 时 host 合成 mp3 → audio_out 决定 设备(mpg123)/页面(浏览器)。
//   - 图片：generate_image 工具产图 → image_out 决定 设备屏(ShowImage)/页面(聊天配图)。

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/config"
	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/minimax"
	"github.com/kingGang/ElectronStudio/internal/protocol"
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
	if io.TTSEngine == "minimax" && a.mm != nil {
		sctx, cancel := context.WithCancel(context.Background())
		a.setSpeakCancel(cancel)
		mp3, err := a.mm.Synthesize(sctx, text, minimax.SpeakOptions{Model: io.MM.TTSModel, VoiceID: io.MM.VoiceID})
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
