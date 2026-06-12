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

// ---------------------------------------------------------------------------
// add_model / remove_model —— 设置页管理大模型
// ---------------------------------------------------------------------------

// AddModelCommand 新增或编辑一个大模型条目（ID 为空表示新增）。
// 注意：模型种类字段的 Go 名为 Kind（JSON 仍为 "type"），以避免与 Payload.Type() 方法重名。
type AddModelCommand struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Kind    string `json:"type"` // echo | openai
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Type 实现 Payload。
func (AddModelCommand) Type() Type { return TypeAddModel }

// RemoveModelCommand 删除一个大模型条目。
type RemoveModelCommand struct {
	ID string `json:"id"`
}

// Type 实现 Payload。
func (RemoveModelCommand) Type() Type { return TypeRemoveModel }

// ---------------------------------------------------------------------------
// 动作编排 / 示教录制
// ---------------------------------------------------------------------------

// FollowCommand 开关"跟随设备"：开启时机器人舵机松力，可手动摆姿，后端持续读回真实角度。
type FollowCommand struct {
	Enable bool `json:"enable"`
}

// Type 实现 Payload。
func (FollowCommand) Type() Type { return TypeFollow }

// RecordStartCommand 开始录制一段以 Name 命名的动作。
type RecordStartCommand struct {
	Name string `json:"name"`
}

// Type 实现 Payload。
func (RecordStartCommand) Type() Type { return TypeRecordStart }

// RecordFrameCommand 把当前姿态采集为一个关键帧（无参数）。
type RecordFrameCommand struct{}

// Type 实现 Payload。
func (RecordFrameCommand) Type() Type { return TypeRecordFrame }

// RecordStopCommand 结束录制并保存（无参数）。
type RecordStopCommand struct{}

// Type 实现 Payload。
func (RecordStopCommand) Type() Type { return TypeRecordStop }

// DeleteActionCommand 删除一段已存在的动作。
type DeleteActionCommand struct {
	Name string `json:"name"`
}

// Type 实现 Payload。
func (DeleteActionCommand) Type() Type { return TypeDeleteAction }

// CameraCommand 开关屏幕显示摄像头画面（开启时屏幕/UI 镜像显示实时摄像头）。
type CameraCommand struct {
	Enable bool `json:"enable"`
}

// Type 实现 Payload。
func (CameraCommand) Type() Type { return TypeCamera }

// GreetCommand 触发"看一眼打招呼"（无参数）。
type GreetCommand struct{}

// Type 实现 Payload。
func (GreetCommand) Type() Type { return TypeGreet }

// MusicCommand 控制音乐播放。
type MusicCommand struct {
	Action string `json:"action"`           // play | pause | resume | stop | volume
	Query  string `json:"query,omitempty"`  // action=play 时的搜索关键词
	Volume int    `json:"volume,omitempty"` // action=volume 时的音量(0~100)
}

// Type 实现 Payload。
func (MusicCommand) Type() Type { return TypeMusic }
