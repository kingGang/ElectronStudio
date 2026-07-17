/**
 * protocol.ts —— 前后端 WebSocket 消息协议（前端镜像）
 *
 * 本文件是 Go 端 `internal/protocol` 的 TypeScript 对应物，二者必须保持一致：
 * 修改协议时请同步更新 Go 包、本文件与 docs/PROTOCOL.md。
 *
 * 通道划分：
 *   - 控制 / 事件：JSON 文本帧，统一封装在 Envelope，按 `type` 分发。
 *   - 屏幕镜像帧：二进制帧（见 decodeFrame），不走 JSON。
 */

// ===========================================================================
// 消息类型常量
// ===========================================================================

/** 服务端 → 客户端（事件） */
export const ServerType = {
  Status: 'status',
  VoiceState: 'voice_state',
  VAD: 'vad',
  Wake: 'wake',
  ASR: 'asr',
  Chat: 'chat',
  TTS: 'tts',
  Emotion: 'emotion',
  Joints: 'joints',
  Error: 'error',
  Gesture: 'gesture',
  MusicState: 'music_state',
  ScheduleList: 'schedule_list',
  Materials: 'materials',
  Audio: 'audio',
  Log: 'log',
} as const

/** 客户端 → 服务端（命令） */
export const ClientType = {
  SendText: 'send_text',
  Mic: 'mic',
  Interrupt: 'interrupt',
  PlayAction: 'play_action',
  SetEmotion: 'set_emotion',
  SelectModel: 'select_model',
  JogJoint: 'jog_joint',
  AddModel: 'add_model',
  RemoveModel: 'remove_model',
  Follow: 'follow',
  RecordStart: 'record_start',
  RecordFrame: 'record_frame',
  RecordStop: 'record_stop',
  DeleteAction: 'delete_action',
  Camera: 'camera',
  Greet: 'greet',
  Music: 'music',
  ScheduleAdd: 'schedule_add',
  ScheduleRemove: 'schedule_remove',
  MaterialDelete: 'material_delete',
  SetIO: 'set_io',
  SetDevice: 'set_device',
  SetVolume: 'set_volume',
  SetRealtime: 'set_realtime',
  Party: 'party',
  Reenable: 'reenable',
  RebootDevice: 'reboot_device',
} as const

export interface SetIOCommand {
  audio_in?: string
  audio_out?: string
  tts_engine?: string
  image_out?: string
  servo_enable?: boolean // 舵机总开关；省略=不改动（false 是有意义的取值：卸力）
}

// 更新实时语音对话配置（设置页）。空/省略字段表示不改动。
// 改动 ws_base/model/api_key 会触发后端热重建并结束当前会话。
export interface SetRealtimeCommand {
  enabled?: boolean // 省略=不改动（false 是有意义的取值：关实时）
  provider?: string
  ws_base?: string
  model?: string
  api_key?: string  // 空=不改动（避免脱敏回显把已存 key 清空）
  voice?: string
}

export interface ScheduleAddCommand {
  title: string
  at?: string
  every?: string
  daily?: string
  kind: string
  text?: string
  query?: string
}

export interface MusicCommand {
  action: 'play' | 'pause' | 'resume' | 'stop' | 'next' | 'prev' | 'volume' | 'report'
  query?: string
  volume?: number
  position?: number // action=report：页面上报的播放进度(秒)
  playing?: boolean // action=report：页面上报的播放/暂停
}

// ===========================================================================
// 共享枚举
// ===========================================================================

export type VoiceState = 'idle' | 'connecting' | 'listening' | 'thinking' | 'speaking'
export type ChatRole = 'user' | 'assistant' | 'system'
export type ChatStatus = 'streaming' | 'final'
export type Emotion = 'neutral' | 'happy' | 'sad' | 'angry' | 'surprised' | 'confused'
export type TTSState = 'start' | 'playing' | 'stop'
export type MicAction = 'start' | 'stop'

/** ElectronBot 舵机数量（6 轴） */
export const JOINT_COUNT = 6

// ===========================================================================
// 信封
// ===========================================================================

/** 所有 JSON 消息的统一信封。 */
export interface Envelope<T = unknown> {
  type: string
  seq?: number
  ts: number
  payload?: T
}

// ===========================================================================
// 服务端 → 客户端 事件负载
// ===========================================================================

export interface RobotStatus {
  connected: boolean
  stuck?: boolean // 已连接但持续无就绪包(疑似固件卡死)，需断电复位
  speed?: string // USB 连接速度，如 "USB 2.0"/"USB 3.0"
  vid: number
  pid: number
  fps: number
}
export interface ServiceStatus {
  running: boolean
  detail?: string
}
export interface ModelInfo {
  id: string
  name: string
  provider: string
}
export interface LLMStatus {
  active: string
  available: ModelInfo[]
}
export interface StatusEvent {
  robot: RobotStatus
  asr: ServiceStatus
  tts: ServiceStatus
  llm: LLMStatus
  actions?: string[] // 可用的编排动作名（供动作编排页使用）
  camera?: boolean    // 是否配置了摄像头
  camera_on?: boolean // 摄像头当前是否开启（前端据此同步开关）
  io?: IOStatus      // I/O 路由当前配置
  realtime?: RealtimeStatus // 实时语音对话当前配置（供设置页展示/编辑）
  music?: MusicStatus // 音乐子系统状态（音源）
  persona?: string   // 设备角色/人设
  voice?: string     // 声音音色
}
export interface SetDeviceCommand {
  persona?: string
  voice?: string
}
export interface MusicStatus {
  source: string // qq | kuwo
  logged_in?: boolean // QQ 音乐是否已登录（音源为 qq 且有 cookie）
}
export interface IOStatus {
  audio_in: string      // device | page | network | off
  audio_out: string     // device | page | both | off
  tts_engine: string    // minimax | openai | sidecar
  image_out: string     // device | page | both | off
  device_volume: number // 设备扬声器音量 0~100
  servo_enable: boolean // 舵机总开关：false 时不上扭矩（可手动摆姿）
}

// 实时语音对话当前配置（供设置页显示与编辑）。
// 出于安全【不回传 API key 明文】——只用 has_key 报告是否已配置。
export interface RealtimeStatus {
  enabled: boolean
  provider?: string
  ws_base?: string
  model?: string
  voice?: string
  has_key: boolean // 是否已配置 API key（不回传明文）
}

export interface VoiceStateEvent {
  state: VoiceState
  detail?: string
}
export interface VADEvent {
  speaking: boolean
  level?: number
}
export interface WakeEvent {
  keyword: string
}
export interface ASREvent {
  text: string
  final: boolean
}
export interface ToolCall {
  id: string
  name: string
  arguments?: string
  result?: string
  status?: string
}
export interface ChatEvent {
  id: string
  role: ChatRole
  content: string
  tools?: ToolCall[]
  images?: string[] // 随消息展示的图片 URL（如 MiniMax 生成图）
  audio?: string    // 随消息展示的音频播放器 URL（如 MiniMax 生成的音乐）
  status: ChatStatus
}
export interface AudioEvent {
  format?: string // mp3 | wav ...
  data?: string   // base64 音频（小段语音）
  url?: string    // 或 HTTP 取回地址（较大音频如音乐）
  text?: string
  stop?: boolean  // true=停止页面当前播放（barge-in）
}
export interface TTSEvent {
  state: TTSState
  text?: string
  sentence_id?: number
}
export interface EmotionEvent {
  emotion: Emotion
}
export interface JointsEvent {
  angles: number[] // 长度为 JOINT_COUNT
  enabled: boolean
}
export interface ErrorEvent {
  code: string
  message: string
}
export interface LogEvent {
  level: string
  message: string
}
export interface MusicStateEvent {
  state: 'playing' | 'paused' | 'stopped'
  name?: string
  artist?: string
  url?: string // 可播放流地址；audio_out=page/both 时由浏览器 <audio> 播放
  position?: number // 起播进度(秒)；刷新/重连恢复时 seek 到该位置
  restore?: boolean // true=重连后的状态恢复
}
export interface MaterialInfo {
  name: string
  frames: number
  fps: number
  kind: string // gif | frames | atlas
}
export interface MaterialsEvent {
  materials: MaterialInfo[]
}

/** 服务端事件的可辨识联合，便于前端按 type 穷尽处理。 */
export type ServerMessage =
  | (Envelope<StatusEvent> & { type: typeof ServerType.Status })
  | (Envelope<VoiceStateEvent> & { type: typeof ServerType.VoiceState })
  | (Envelope<VADEvent> & { type: typeof ServerType.VAD })
  | (Envelope<WakeEvent> & { type: typeof ServerType.Wake })
  | (Envelope<ASREvent> & { type: typeof ServerType.ASR })
  | (Envelope<ChatEvent> & { type: typeof ServerType.Chat })
  | (Envelope<TTSEvent> & { type: typeof ServerType.TTS })
  | (Envelope<EmotionEvent> & { type: typeof ServerType.Emotion })
  | (Envelope<JointsEvent> & { type: typeof ServerType.Joints })
  | (Envelope<ErrorEvent> & { type: typeof ServerType.Error })
  | (Envelope<MaterialsEvent> & { type: typeof ServerType.Materials })
  | (Envelope<AudioEvent> & { type: typeof ServerType.Audio })
  | (Envelope<LogEvent> & { type: typeof ServerType.Log })

// ===========================================================================
// 客户端 → 服务端 命令负载
// ===========================================================================

export interface SendTextCommand {
  text: string
}
export interface MicCommand {
  action: MicAction
}
export interface InterruptCommand {
  reason?: string
}
export interface PlayActionCommand {
  name: string
  loops?: number
}
export interface SetEmotionCommand {
  emotion: Emotion
  preview?: boolean // 仅切屏预览、不联动同名动作（素材预览用）
}
export interface SelectModelCommand {
  id: string
}
export interface JogJointCommand {
  joint: number
  angle: number
  enable: boolean
}
export interface AddModelCommand {
  id?: string
  name: string
  type: string // echo | openai
  base_url?: string
  api_key?: string
  model?: string
}
export interface RemoveModelCommand {
  id: string
}
export interface FollowCommand {
  enable: boolean
}
export interface RecordStartCommand {
  name: string
}
export interface DeleteActionCommand {
  name: string
}
export interface MaterialDeleteCommand {
  name: string
}

// ===========================================================================
// 编解码辅助
// ===========================================================================

/** 将一条命令封装为可通过 WebSocket 文本帧发送的 JSON 字符串。 */
export function encode<T>(type: string, payload?: T): string {
  const env: Envelope<T> = { type, ts: Date.now(), payload }
  return JSON.stringify(env)
}

/** 解析收到的 JSON 文本帧为服务端消息；非法数据返回 null。 */
export function decode(raw: string): ServerMessage | null {
  try {
    const env = JSON.parse(raw) as Envelope
    if (!env || typeof env.type !== 'string') return null
    return env as ServerMessage
  } catch {
    return null
  }
}

// ===========================================================================
// 屏幕镜像帧（二进制）
// ===========================================================================

/** 二进制帧头固定长度（字节），与 Go 端 FrameHeaderSize 一致。 */
export const FRAME_HEADER_SIZE = 14

export const PixelFormat = {
  RGB888: 1,
  RGB565: 2,
} as const

export interface FrameHeader {
  width: number
  height: number
  format: number
  flags: number
  seq: number
}

export interface DecodedFrame {
  header: FrameHeader
  /** 像素数据（引用底层 buffer，未复制） */
  pixels: Uint8Array
}

/**
 * 解析一个 WebSocket 二进制帧（ArrayBuffer）为帧头 + 像素数据。
 * 校验魔数 "EBF1" 与长度，失败返回 null。
 */
export function decodeFrame(buf: ArrayBuffer): DecodedFrame | null {
  if (buf.byteLength < FRAME_HEADER_SIZE) return null
  const view = new DataView(buf)
  // 魔数 "EBF1"
  if (
    view.getUint8(0) !== 0x45 || // 'E'
    view.getUint8(1) !== 0x42 || // 'B'
    view.getUint8(2) !== 0x46 || // 'F'
    view.getUint8(3) !== 0x31 // '1'
  ) {
    return null
  }
  const header: FrameHeader = {
    width: view.getUint16(4, true),
    height: view.getUint16(6, true),
    format: view.getUint8(8),
    flags: view.getUint8(9),
    seq: view.getUint32(10, true),
  }
  const pixels = new Uint8Array(buf, FRAME_HEADER_SIZE)
  return { header, pixels }
}
