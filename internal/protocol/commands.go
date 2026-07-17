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
	// Preview 为 true 时只切换屏幕表情画面、广播情绪，不联动播放同名编排动作。
	// 素材管理页「预览」用它，避免预览一个与动作同名的素材时让真机做出物理动作。
	Preview bool `json:"preview,omitempty"`
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

// PartyCommand 触发"一键蹦迪"：后端同时放歌 + 循环跳 dance（踩拍变脸）。
// Query 为空时用内置默认曲目；停止用 interrupt + 音乐 stop。
type PartyCommand struct {
	Query string `json:"query,omitempty"` // 可选曲目关键词；空则用默认
}

// Type 实现 Payload。
func (PartyCommand) Type() Type { return TypeParty }

// ScheduleAddCommand 新增定时任务/提醒。At/Every/Daily 三选一。
type ScheduleAddCommand struct {
	Title string `json:"title"`
	At    string `json:"at,omitempty"`    // RFC3339 一次性
	Every string `json:"every,omitempty"` // 时长，如 1h
	Daily string `json:"daily,omitempty"` // HH:MM 每日
	Kind  string `json:"kind"`            // say | weather | greet | music
	Text  string `json:"text,omitempty"`  // say 内容
	Query string `json:"query,omitempty"` // weather 城市 / music 歌名
}

// Type 实现 Payload。
func (ScheduleAddCommand) Type() Type { return TypeScheduleAdd }

// ScheduleRemoveCommand 删除一个定时任务。
type ScheduleRemoveCommand struct {
	ID string `json:"id"`
}

// Type 实现 Payload。
func (ScheduleRemoveCommand) Type() Type { return TypeScheduleRemove }

// SetIOCommand 更新 I/O 路由配置（设置页）。空字段表示不改动。
type SetIOCommand struct {
	AudioIn   string `json:"audio_in,omitempty"`
	AudioOut  string `json:"audio_out,omitempty"`
	TTSEngine string `json:"tts_engine,omitempty"`
	ImageOut  string `json:"image_out,omitempty"`
	// ServoEnable 用指针：舵机总开关的 false 是有意义的取值（关扭矩），
	// 不能与"本次命令没带这个字段"混为一谈，故 nil 才表示不改动。
	ServoEnable *bool `json:"servo_enable,omitempty"`
}

// Type 实现 Payload。
func (SetIOCommand) Type() Type { return TypeSetIO }

// SetRealtimeCommand 更新实时语音对话配置（设置页）。nil/空字段表示不改动。
// 改动后端点/model/key 会触发后端热重建；正在进行的实时会话会被结束。
type SetRealtimeCommand struct {
	Enabled  *bool  `json:"enabled,omitempty"` // 指针：false 是有意义的取值（关实时）
	Provider string `json:"provider,omitempty"`
	WSBase   string `json:"ws_base,omitempty"`
	Model    string `json:"model,omitempty"`
	APIKey   string `json:"api_key,omitempty"` // 空=不改动（避免脱敏回显把 key 清空）
	Voice    string `json:"voice,omitempty"`
}

// Type 实现 Payload。
func (SetRealtimeCommand) Type() Type { return TypeSetRealtime }

// MaterialDeleteCommand 删除一段屏幕表情素材（按情绪名）。上传走 HTTP（POST /api/materials）。
type MaterialDeleteCommand struct {
	Name string `json:"name"`
}

// Type 实现 Payload。
func (MaterialDeleteCommand) Type() Type { return TypeMaterialDelete }

// MusicCommand 控制音乐播放。
type MusicCommand struct {
	Action   string  `json:"action"`             // play | pause | resume | stop | next | prev | volume | report
	Query    string  `json:"query,omitempty"`    // action=play 时的搜索关键词
	Volume   int     `json:"volume,omitempty"`   // action=volume 时的音量(0~100)
	Position float64 `json:"position,omitempty"` // action=report 时页面上报的播放进度(秒)
	Playing  bool    `json:"playing,omitempty"`  // action=report 时页面上报的播放/暂停状态
}

// Type 实现 Payload。
func (MusicCommand) Type() Type { return TypeMusic }

// SetDeviceCommand 设置设备角色(人设)与声音音色。空字段表示不改。
type SetDeviceCommand struct {
	Persona       string `json:"persona,omitempty"`        // 角色/人设（系统提示的人设部分）
	PersonaSource string `json:"persona_source,omitempty"` // local | model（小智用自带角色）
	Voice         string `json:"voice,omitempty"`          // 声音音色（覆盖当前 TTS 引擎音色）
}

// Type 实现 Payload。
func (SetDeviceCommand) Type() Type { return TypeSetDevice }

// SetVolumeCommand 设置设备扬声器音量(0~100)，设置页滑块用。
type SetVolumeCommand struct {
	Volume int `json:"volume"`
}

// Type 实现 Payload。
func (SetVolumeCommand) Type() Type { return TypeSetVolume }

// ---------------------------------------------------------------------------
// reenable —— 舵机重新上扭矩
// ---------------------------------------------------------------------------

// ReenableCommand 让驱动下发一次 enable 0→1 跳变，给舵机重新上扭矩。
// 舵机的过流/堵转保护一旦锁存，就会"能应答 I²C、能报位置，但电机不转"，只有重新使能能解锁。
type ReenableCommand struct{}

// Type 实现 Payload。
func (ReenableCommand) Type() Type { return TypeReenable }

// ---------------------------------------------------------------------------
// reboot_device —— 串口软复位设备（免拔电源）
// ---------------------------------------------------------------------------

// RebootDeviceCommand 触发设备软复位：往 ElectronBot 的 CP210x/CH340 串口发一条复位指令，
// 使 MCU 系统复位并重新枚举 USB——【免拔电源】。对应官方 ElectronBot.DotNet 的"复位电子"按钮。
// 固件卡死(bulk 无就绪包)时驱动也会自动软复位；这个命令是手动触发入口。无参数。
type RebootDeviceCommand struct{}

// Type 实现 Payload。
func (RebootDeviceCommand) Type() Type { return TypeRebootDevice }
