// Package protocol 定义前端（Web）与后端（Go）之间通过 WebSocket 交换的
// 全部消息契约，是前后端通信的【单一事实来源】。
//
// 传输划分为两类通道：
//
//   - 控制 / 事件消息：以 JSON 文本帧传输，统一封装在 Envelope 中，
//     通过鉴别字段 Type 区分具体负载。定义见 events.go（服务端→客户端）
//     与 commands.go（客户端→服务端）。
//   - 屏幕镜像帧：240×240 像素数据量大且高频，以 WebSocket 二进制帧传输，
//     使用紧凑的二进制头，避免 base64 膨胀。定义见 frame.go。
//
// 任何对消息结构的修改都应在本包完成，并同步到 web/src/protocol.ts 与
// docs/PROTOCOL.md，确保三处一致。
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Type 是消息的鉴别字段（discriminator）。所有取值集中定义在本文件中，
// 以便一眼看清协议的完整面貌；具体负载结构分散在 events.go / commands.go。
type Type string

// 服务端 → 客户端（事件）。
const (
	TypeStatus     Type = "status"      // 连接与各子服务（USB/ASR/TTS/LLM）状态快照
	TypeVoiceState Type = "voice_state" // 语音状态机变化（idle/listening/speaking…）
	TypeVAD        Type = "vad"         // 语音活动检测：是否在说话 + 波形电平
	TypeWake       Type = "wake"        // 检测到唤醒词
	TypeASR        Type = "asr"         // 语音识别结果（中间态 / 最终态）
	TypeChat       Type = "chat"        // 一条对话消息（流式或最终），含工具调用
	TypeTTS        Type = "tts"         // 语音合成播放状态
	TypeEmotion    Type = "emotion"     // 当前情绪变化
	TypeJoints     Type = "joints"      // 6 轴舵机真实角度反馈
	TypeError      Type = "error"       // 错误通知
	TypeGesture      Type = "gesture"       // 识别到手势
	TypeMusicState   Type = "music_state"   // 音乐播放状态变化
	TypeScheduleList Type = "schedule_list" // 定时任务列表
	TypeLog          Type = "log"           // 调试日志（可选，供前端日志面板使用）
)

// 客户端 → 服务端（命令）。
const (
	TypeSendText    Type = "send_text"    // 用户发送文本消息
	TypeMic         Type = "mic"          // 控制麦克风（按住说话 / 开关）
	TypeInterrupt   Type = "interrupt"    // 打断当前回应或动作
	TypePlayAction  Type = "play_action"  // 触发一段编排动作
	TypeSetEmotion  Type = "set_emotion"  // 手动设置情绪
	TypeSelectModel Type = "select_model" // 切换当前大模型
	TypeJogJoint    Type = "jog_joint"    // 手动微调单个舵机角度
	TypeAddModel    Type = "add_model"    // 新增/编辑一个大模型（设置页）
	TypeRemoveModel Type = "remove_model" // 删除一个大模型（设置页）

	// 动作编排 / 示教录制
	TypeFollow       Type = "follow"        // 开关"跟随设备"（实体优先：松力 + 读回角度）
	TypeRecordStart  Type = "record_start"  // 开始录制一段动作
	TypeRecordFrame  Type = "record_frame"  // 采集当前姿态为一帧
	TypeRecordStop   Type = "record_stop"   // 结束录制并保存
	TypeDeleteAction Type = "delete_action" // 删除一段动作
	TypeCamera       Type = "camera"        // 开关屏幕显示摄像头画面
	TypeGreet          Type = "greet"           // 看一眼并主动打招呼
	TypeMusic          Type = "music"           // 音乐控制（播放/暂停/停止/音量）
	TypeScheduleAdd    Type = "schedule_add"    // 新增定时任务/提醒
	TypeScheduleRemove Type = "schedule_remove" // 删除定时任务
)

// Payload 是所有可被封装进 Envelope 的负载结构的统一约束。
// 每个负载结构都通过实现 Type() 声明自己对应的消息类型，
// 从而让编解码层无需手工维护「结构 ↔ 类型」映射，杜绝二者不一致。
type Payload interface {
	Type() Type
}

// Envelope 是 JSON 文本消息的统一信封。
type Envelope struct {
	// Type 标识负载类型，是解码时的分发依据。
	Type Type `json:"type"`
	// Seq 为可选自增序号，用于请求/响应配对与问题排查；不需要时可为 0。
	Seq uint64 `json:"seq,omitempty"`
	// TS 为消息生成时的 Unix 毫秒时间戳。
	TS int64 `json:"ts"`
	// Payload 为与 Type 对应的具体负载，延迟解析（先读类型再按需反序列化）。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ErrEmptyType 表示信封缺少 Type 字段，无法分发。
var ErrEmptyType = errors.New("protocol: 信封缺少 type 字段")

// Validate 校验信封自身的合法性（不校验 Payload 内部字段）。
func (e Envelope) Validate() error {
	if e.Type == "" {
		return ErrEmptyType
	}
	return nil
}

// Encode 将一个负载封装为可直接通过 WebSocket 文本帧发送的 JSON 字节流。
// 信封的 Type 自动取自负载的 Type()，TS 自动填充为当前时间，避免人工出错。
func Encode(p Payload) ([]byte, error) {
	if p == nil {
		return nil, errors.New("protocol: 负载不能为 nil")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("protocol: 序列化负载 %q 失败: %w", p.Type(), err)
	}
	env := Envelope{
		Type:    p.Type(),
		TS:      time.Now().UnixMilli(),
		Payload: raw,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("protocol: 序列化信封 %q 失败: %w", p.Type(), err)
	}
	return data, nil
}

// EncodeSeq 与 Encode 相同，但额外写入序号，便于请求/响应配对。
func EncodeSeq(p Payload, seq uint64) ([]byte, error) {
	data, err := Encode(p)
	if err != nil {
		return nil, err
	}
	// 复用 Encode 的结果再注入 seq，保持单一编码路径，避免逻辑重复。
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	env.Seq = seq
	return json.Marshal(env)
}

// Decode 将收到的 JSON 字节流解析为信封。返回后应根据 Envelope.Type
// 调用 As 将 Payload 反序列化为具体结构。
func Decode(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("protocol: 解析信封失败: %w", err)
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// As 将信封中的 Payload 反序列化为指定的负载类型 T。
// 它会先校验信封的 Type 与 T 声明的类型一致，再做反序列化，
// 因此能在类型层面拦截「类型对不上负载」的脏数据。
//
// 用法：
//
//	env, _ := protocol.Decode(data)
//	switch env.Type {
//	case protocol.TypeSendText:
//	    cmd, err := protocol.As[SendTextCommand](env)
//	    ...
//	}
func As[T Payload](env Envelope) (T, error) {
	var v T
	if want := v.Type(); env.Type != want {
		return v, fmt.Errorf("protocol: 类型不匹配, 信封为 %q 但期望 %q", env.Type, want)
	}
	if len(env.Payload) == 0 {
		// 无负载体也是合法的（如某些无参命令）；返回零值即可。
		return v, nil
	}
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		return v, fmt.Errorf("protocol: 反序列化负载 %q 失败: %w", env.Type, err)
	}
	return v, nil
}
