package llm

import (
	"context"
	"log/slog"
	"sync"

	"github.com/kingGang/ElectronStudio/internal/xiaozhi"
)

// XiaozhiProvider 把小智(xiaozhi) AI 作为一个对话后端接入。
// 它是"文字进/文字出"：取最后一条用户消息发给小智，收集回复文本返回。
// 小智自带 ASR/LLM/TTS，本端不再做工具循环（SupportsTools=false），回复文本可由
// 现有 TTS 引擎朗读。最近一次情绪可经 LastEmotion 读取（供上层联动表情）。
type XiaozhiProvider struct {
	info      Info
	client    *xiaozhi.Client
	emo       string
	personaFn func() string      // 返回要注入的本机角色（空=用小智自带角色）；由上层按 persona_source 设置
	audioSink func(ogg []byte)   // 非空时：流式逐句把音频(Ogg/Opus)送出，由上层即时播放

	mu    sync.Mutex
	audio []byte // 最近一次回复自带的 TTS 音频（Ogg/Opus）；被 LastAudioOgg 取走后清空（仅非流式路径用）
}

// NewXiaozhi 创建小智 Provider。
func NewXiaozhi(id, name string, cfg xiaozhi.Config, log *slog.Logger) *XiaozhiProvider {
	return &XiaozhiProvider{
		info:   Info{ID: id, Name: name, Provider: "xiaozhi"},
		client: xiaozhi.New(cfg, log),
	}
}

// SetPersonaFn 设置"本机角色"取值函数（persona_source=local 时返回角色文本，否则返回空）。
func (p *XiaozhiProvider) SetPersonaFn(fn func() string) { p.personaFn = fn }

// SetAudioSink 设置流式音频回调：设置后，Chat 会在小智每说完一句时把该句音频(Ogg/Opus)
// 即时交给 sink 播放（边收边播）；不设则音频在收齐后随回复一次性给出（旧行为）。
func (p *XiaozhiProvider) SetAudioSink(fn func(ogg []byte)) { p.audioSink = fn }

func (p *XiaozhiProvider) persona() string {
	if p.personaFn == nil {
		return ""
	}
	return p.personaFn()
}

// Info 实现 Provider。
func (p *XiaozhiProvider) Info() Info { return p.info }

// SupportsTools 实现 Provider：小智自行处理意图，本端不做 function-calling。
func (p *XiaozhiProvider) SupportsTools() bool { return false }

// LastEmotion 返回最近一次回复带的情绪（可空）。
func (p *XiaozhiProvider) LastEmotion() string { return p.emo }

// LastAudioOgg 取走最近一次回复自带的 TTS 音频（Ogg/Opus），并清空——
// 一次性消费，避免后续非小智轮次（如放歌确认语）误重放旧音频。实现 llm.AudioReplier。
func (p *XiaozhiProvider) LastAudioOgg() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.audio
	p.audio = nil
	return a
}

func (p *XiaozhiProvider) setAudio(a []byte) {
	p.mu.Lock()
	p.audio = a
	p.mu.Unlock()
}

func lastUser(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// Complete 实现 Provider：发一句、取完整回复。
func (p *XiaozhiProvider) Complete(ctx context.Context, req Request) (Completion, error) {
	r, err := p.client.Ask(ctx, lastUser(req.Messages), p.persona())
	if err != nil {
		return Completion{}, err
	}
	p.emo = r.Emotion
	p.setAudio(r.Audio)
	return Completion{Content: r.Text}, nil
}

// Chat 实现 Provider：流式接口下，小智一次性返回，这里整段作为一个增量发出。
func (p *XiaozhiProvider) Chat(ctx context.Context, req Request) (<-chan Chunk, error) {
	ch := make(chan Chunk, 8)
	go func() {
		defer close(ch)
		// 逐句把文本流式送出，降低首字延迟；音频在收齐后随 Reply 返回再统一播放。
		onText := func(t string) {
			select {
			case ch <- Chunk{Delta: t}:
			case <-ctx.Done():
			}
		}
		r, err := p.client.AskStream(ctx, lastUser(req.Messages), p.persona(), onText, p.audioSink)
		if err != nil {
			ch <- Chunk{Err: err}
			return
		}
		p.emo = r.Emotion
		p.setAudio(r.Audio) // audioSink 非空时 r.Audio 为 nil（已流式播放）
		ch <- Chunk{Done: true}
	}()
	return ch, nil
}
