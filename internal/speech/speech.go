// Package speech 抽象本地语音能力（唤醒 / VAD / ASR / TTS）。
//
// 受"主程序不依赖 C 工具链"约束，真正的语音模型（sherpa-onnx、Piper 等）以
// 独立的 sidecar 进程运行，由它们直接占用麦克风与扬声器；本包通过一条本地
// WebSocket 与 sidecar 交互：
//
//	sidecar → 本包：wake / vad / asr 事件（上行语音输入）
//	本包 → sidecar：speak / abort（下行语音合成请求）
//
// 这样 Go 侧完全不碰音频设备，保持纯 Go、可交叉编译。无 sidecar 时使用
// Mock 实现，链路（文本输入→对话）依旧可跑通。协议见 docs/SPEECH.md。
package speech

import "context"

// EventKind 标识一条上行语音事件的类型。
type EventKind string

const (
	KindWake  EventKind = "wake"  // 命中唤醒词
	KindVAD   EventKind = "vad"   // 语音活动变化
	KindASR   EventKind = "asr"   // 语音识别结果
	KindAudio EventKind = "audio" // realtime 上行：麦克风原始 PCM（16k/单声道/int16）
)

// Event 是来自 sidecar 的一条上行语音事件。
// 不同 Kind 使用的字段不同：
//   - Wake ：Keyword
//   - VAD  ：Speaking、Level
//   - ASR  ：Text、Final
//   - Audio：PCM（realtime 模式的原始麦克风音频，转发给云端语音大模型）
type Event struct {
	Kind     EventKind
	Keyword  string  // Wake：命中的唤醒词
	Speaking bool    // VAD：当前是否在说话
	Level    float32 // VAD：归一化电平 [0,1]
	Text     string  // ASR：识别文本
	Final    bool    // ASR：是否为最终结果
	PCM      []byte  // Audio：原始 PCM 字节（16k/单声道/int16）
}

// Status 描述语音子服务的运行状态，用于上报给前端。
type Status struct {
	ASRRunning bool
	TTSRunning bool
	Detail     string
	Voice      Voice
}

// Voice 是 sidecar 侧 TTS 的音色能力与当前取值，由 sidecar 连上时上报（"voice" 消息）。
//
// Speakers 是【本模型】的音色总数，界面据此限定可选范围——这不是可以写死的常量：
// 多说话人模型(VITS-zh fanchen-C)有 187 个音色，单说话人模型(Piper huayan)只有 1 个、
// 此时 SpeakerID 只能是 0，界面应据此把选择器藏起来或置灰。Speakers=0 表示尚未上报。
type Voice struct {
	Speakers  int     // 音色总数（0=未知/未连接）
	SpeakerID int     // 当前音色，取值 0..Speakers-1
	Speed     float64 // 语速，1.0 为原速
}

// Service 抽象一套完整的本地语音能力。
type Service interface {
	// Start 启动服务（如连接 sidecar）。应尊重 ctx 取消。
	Start(ctx context.Context) error
	// Events 返回上行语音事件流（唤醒 / VAD / ASR）。
	Events() <-chan Event
	// Speak 请求把文本合成为语音并播放。
	Speak(ctx context.Context, text string) error
	// Stop 打断当前正在播放的语音。
	Stop()
	// SetVoice 换 TTS 音色/语速。sid<0 或 speed<=0 表示"该项不变"。
	// sidecar 每次合成都带 sid/speed，故即时生效、无需重载模型或重启。
	SetVoice(ctx context.Context, sid int, speed float64) error
	// Status 返回当前语音子服务状态。
	Status() Status
	// Close 释放资源。
	Close() error
}
