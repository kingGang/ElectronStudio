package protocol

// 本文件定义【客户端 → 服务端】方向的命令负载。
// 每个结构都实现 Payload.Type()，与 protocol.go 中的 Type 常量一一对应。

// ---------------------------------------------------------------------------
// send_text —— 文本输入
// ---------------------------------------------------------------------------

// SendTextCommand 表示用户在输入框发送了一条文本消息（等价于说话）。
type SendTextCommand struct {
	Text string `json:"text"`
}

// Type 实现 Payload。
func (SendTextCommand) Type() Type { return TypeSendText }

// ---------------------------------------------------------------------------
// mic —— 麦克风控制
// ---------------------------------------------------------------------------

// MicAction 表示对麦克风的控制动作。
type MicAction string

const (
	MicStart MicAction = "start" // 开始拾音（按住说话按下 / 开启常听）
	MicStop  MicAction = "stop"  // 停止拾音（松开 / 关闭常听）
)

// MicCommand 控制麦克风拾音的开关。
type MicCommand struct {
	Action MicAction `json:"action"`
}

// Type 实现 Payload。
func (MicCommand) Type() Type { return TypeMic }

// ---------------------------------------------------------------------------
// interrupt —— 打断
// ---------------------------------------------------------------------------

// InterruptCommand 请求打断当前的回应或动作（对应 TTS 播放中的 barge-in）。
type InterruptCommand struct {
	Reason string `json:"reason,omitempty"` // 可选: 打断原因, 便于日志追踪
}

// Type 实现 Payload。
func (InterruptCommand) Type() Type { return TypeInterrupt }

// ---------------------------------------------------------------------------
// play_action —— 触发编排动作
// ---------------------------------------------------------------------------

// PlayActionCommand 触发一段预定义的编排动作（如「打招呼」「点头」）。
type PlayActionCommand struct {
	Name  string `json:"name"`            // 动作名, 对应 choreography 中注册的动作
	Loops int    `json:"loops,omitempty"` // 循环次数, 0 表示按动作默认值, -1 表示无限循环
}

// Type 实现 Payload。
func (PlayActionCommand) Type() Type { return TypePlayAction }

// ---------------------------------------------------------------------------
// set_emotion —— 手动设置情绪
// ---------------------------------------------------------------------------

// SetEmotionCommand 手动切换机器人情绪（同时影响表情画面）。
type SetEmotionCommand struct {
	Emotion Emotion `json:"emotion"`
}

// Type 实现 Payload。
func (SetEmotionCommand) Type() Type { return TypeSetEmotion }

// ---------------------------------------------------------------------------
// select_model —— 切换大模型
// ---------------------------------------------------------------------------

// SelectModelCommand 切换当前使用的大模型。
type SelectModelCommand struct {
	ID string `json:"id"` // 目标模型 ID, 取自 StatusEvent.LLM.Available
}

// Type 实现 Payload。
func (SelectModelCommand) Type() Type { return TypeSelectModel }

// ---------------------------------------------------------------------------
// jog_joint —— 手动微调舵机
// ---------------------------------------------------------------------------

// JogJointCommand 手动设置单个舵机的目标角度（动作编排页的手动调试用）。
type JogJointCommand struct {
	Joint  int     `json:"joint"`  // 舵机序号 [0, JointCount)
	Angle  float32 `json:"angle"`  // 目标角度（度）
	Enable bool    `json:"enable"` // 是否使能该舵机
}

// Type 实现 Payload。
func (JogJointCommand) Type() Type { return TypeJogJoint }
