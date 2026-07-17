# 前后端通信协议（WebSocket）

本文件描述 Web 前端与 Go 后端之间的实时通信契约。

- Go 实现（单一事实来源）：`internal/protocol`
- 前端镜像类型：`web/src/protocol.ts`
- 三处修改须保持同步。

## 通道划分

前后端共用**一条 WebSocket 连接**，按帧类型区分两类消息：

| WebSocket 帧类型 | 用途 | 格式 |
|------------------|------|------|
| 文本帧（Text） | 控制 / 事件 | JSON `Envelope` |
| 二进制帧（Binary） | 屏幕镜像帧（240×240 高频画面） | 紧凑二进制头 + 像素 |

镜像帧单独走二进制，避免 base64 膨胀与高频 JSON 编解码开销。

## JSON 信封

```jsonc
{
  "type": "asr",     // 消息类型（鉴别字段，必填）
  "seq": 42,         // 可选自增序号，用于请求/响应配对与排查
  "ts": 1781170000000, // Unix 毫秒时间戳
  "payload": { }     // 与 type 对应的负载
}
```

解码流程：先读 `type`，再据此把 `payload` 反序列化为对应结构（Go 用 `protocol.As[T]`，前端用可辨识联合）。

## 服务端 → 客户端（事件）

| `type` | 负载 | 说明 |
|--------|------|------|
| `status` | `StatusEvent` | 各子系统（USB / ASR / TTS / LLM）状态快照，连接建立或状态变化时下发。其中 `sidecar_voice` 是 sidecar 本地 TTS 的音色能力：`speakers`=本模型音色总数（**由 sidecar 上报，不可写死**——fanchen-C 是 187、Piper 是 1、0=未连接）、`speaker_id`=当前音色、`speed`=当前语速。注意与 `voice` 字段区分：`voice` 是设备/云端 TTS 的音色**名**（如 MiniMax 的 `male-qn-qingse`），`sidecar_voice` 是本地模型的音色**序号** |
| `voice_state` | `VoiceStateEvent` | 语音状态机：`idle` / `connecting` / `listening` / `thinking` / `speaking` |
| `vad` | `VADEvent` | 语音活动：`speaking` + 归一化电平 `level`（驱动波形） |
| `wake` | `WakeEvent` | 命中唤醒词 |
| `asr` | `ASREvent` | 识别结果，可多次中间态，`final=true` 为最终 |
| `chat` | `ChatEvent` | 一条对话消息；流式时同 `id` 多次下发、`status=streaming` 且 `content` 为累计全文，完成时 `status=final` |
| `tts` | `TTSEvent` | 合成播放状态：`start` / `playing` / `stop` |
| `emotion` | `EmotionEvent` | 当前情绪变化 |
| `joints` | `JointsEvent` | 6 轴舵机真实角度反馈 |
| `error` | `ErrorEvent` | 面向用户的错误（含机器码 `code`） |
| `schedule_list` | `ScheduleListEvent` | 定时任务/提醒列表（增删后广播，连接时推送一次） |
| `materials` | `MaterialsEvent` | 屏幕表情素材列表（上传/删除后广播，连接时推送一次）。列出**机器人支持的全部情绪**，不只是磁盘上有素材的：没上传素材的情绪 `kind="sdf"`（`frames`/`fps` 为 0），由程序脸实时绘制、缩略图由 `/api/material-thumb` 实时渲染、无文件可删；用户自定义命名的素材也一并列出 |
| `log` | `LogEvent` | 调试日志（可选） |

### 示例

```jsonc
// 状态快照
{ "type": "status", "ts": 1781170000000, "payload": {
  // robot.stuck=持续无就绪包(疑似固件卡死)；robot.recovering=卡死后正在自动串口软复位(免拔电源)自救中，
  // 为 true 时 UI 显示"自动复位中"而非"请断电"，stuck && !recovering 才是"自动复位无效、需手动断电"。
  "robot": { "connected": true, "stuck": false, "recovering": false, "speed": "USB 2.0", "vid": 4097, "pid": 32803, "fps": 30 },
  "asr":   { "running": true,  "detail": "SenseVoice-zh" },
  "tts":   { "running": true,  "detail": "piper zh_CN-huayan" },
  "llm":   { "active": "ollama:qwen2.5", "available": [
              { "id": "ollama:qwen2.5", "name": "Qwen2.5", "provider": "ollama" },
              { "id": "openai:gpt-4o",  "name": "GPT-4o",  "provider": "openai" } ] }
}}
// status.payload 另含 io: { audio_in, audio_out, tts_engine, image_out, device_volume(0~100),
//   servo_enable(bool，舵机总开关：false 时不上扭矩、可手动摆姿) }。
// 另含 realtime: { enabled(bool), provider, ws_base, model, voice, has_key(bool，是否已配置 key，
//   【不回传明文】) }——实时语音对话当前配置。
// 另含 camera(bool，是否配置摄像头) / camera_on(bool，当前是否开启，前端据此同步切换按钮)。
// 设置类命令（前端→后端，设置页用，均即时落盘并广播新 status）：
//   set_io { audio_in/audio_out/tts_engine/image_out/servo_enable }
//     servo_enable 是 bool 且省略即不改动（false 有意义），其余为字符串、空串即不改动。
//     切换 servo_enable 即时生效：驱动下一帧按新值下发使能位，无需重启。
//   set_device { persona, persona_source, voice }
//   set_volume { volume: 0~100 }    // 设备扬声器音量（macOS 经 playto 设 USB 声卡）
//   set_realtime { enabled, provider, ws_base, model, api_key, voice }
//     enabled 是 bool 且省略即不改动（false 有意义：关实时）；其余为字符串、空串即不改动。
//     api_key 空串=不改动（状态从不回传明文，避免脱敏回显把已存 key 清空）。
//     改 ws_base/model/api_key 会热重建后端并结束当前进行中的实时会话（下次唤醒用新配置）。

// 流式对话（同一 id 多次更新）
{ "type": "chat", "ts": 1781170001000, "payload": {
  "id": "m_18", "role": "assistant", "content": "好的，已为你", "status": "streaming" }}
{ "type": "chat", "ts": 1781170001200, "payload": {
  "id": "m_18", "role": "assistant", "content": "好的，已为你打开台灯",
  "tools": [ { "id": "t1", "name": "lamp.turn_on", "status": "ok" } ],
  "status": "final" }}

// 舵机反馈
{ "type": "joints", "ts": 1781170002000, "payload": {
  "angles": [12.3, -5.0, 47.0, 20.0, 0.0, 0.0], "enabled": true }}
```

## 客户端 → 服务端（命令）

| `type` | 负载 | 说明 |
|--------|------|------|
| `send_text` | `SendTextCommand` | 发送文本消息（等价于说话） |
| `mic` | `MicCommand` | 麦克风控制：`start` / `stop` |
| `interrupt` | `InterruptCommand` | 打断当前回应或动作 |
| `play_action` | `PlayActionCommand` | 触发编排动作（`name` + 可选 `loops`） |
| `set_emotion` | `SetEmotionCommand` | 手动设置情绪 |
| `select_model` | `SelectModelCommand` | 切换大模型（`id` 取自 `status.llm.available`），并持久化为生效模型 |
| `jog_joint` | `JogJointCommand` | 手动微调单个舵机（`joint` / `angle` / `enable`） |
| `add_model` | `AddModelCommand` | 新增/编辑大模型（`name` / `type` / `base_url` / `api_key` / `model`），写入配置文件 |
| `remove_model` | `RemoveModelCommand` | 删除大模型（`id`），写入配置文件 |
| `material_delete` | `MaterialDeleteCommand` | 删除一段屏幕表情素材（`name`），删后热重载并广播 `materials` |
| `party` | `PartyCommand` | 一键蹦迪：同时放歌 + 无限循环跳 `dance`（踩拍变脸），可选 `query` 指定曲目；停止用 `interrupt` + `music`(`stop`) |
| `reenable` | `ReenableCommand` | 给舵机重新上扭矩（下发一次 enable 0→1 跳变）。舵机的过流/堵转保护锁存后会「能应答 I²C、能报位置，但电机不转」，只有重新使能能解锁；驱动也会自动检测并重试 |
| `set_face_style` | `SetFaceStyleCommand` | 切换**表情类型**（"系列"，多套风格并存、热切并落盘）：`b`=类型B（全部走 SDF 程序脸）；`bw`=黑白眼睛类（优先用 `emotions/` 的 GIF 素材，该类型没有的表情自动用类型B补）。当前类型见 `status.face_style` |
| `set_voice` | `SetVoiceCommand` | 换 **sidecar 本地 TTS 音色/语速**，即时生效并落盘。`speaker_id` 的有效范围**取决于 sidecar 装了哪个模型**（VITS-zh fanchen-C 有 187 个音色；Piper huayan 只有 1 个、只能填 0），范围由 `status.sidecar_voice.speakers` 给出，**不要写死**。`preview=true` 只试听不落盘（挑音色时连听十几个，不该每个都写盘）；`preview_text` 留空用默认句 |
| `reboot_device` | `RebootDeviceCommand` | 串口软复位设备（**免拔电源**）：往 ElectronBot 的 CP210x/CH340 串口发复位指令，使 MCU 系统复位并重新枚举 USB，设备掉线 ~6s 后驱动自动重连。对应官方「复位电子」按钮；固件卡死(bulk 持续无就绪包)时驱动也会自动软复位（`io.auto_reboot` 缺省开）。无参数 |

> 注：屏幕表情素材的**上传**是二进制文件，不走 WebSocket，而走下方的 HTTP REST 接口。

### 示例

```jsonc
{ "type": "send_text",    "ts": 1781170003000, "payload": { "text": "帮我开台灯" } }
{ "type": "play_action",  "ts": 1781170003100, "payload": { "name": "wave", "loops": 1 } }
{ "type": "jog_joint",    "ts": 1781170003200, "payload": { "joint": 2, "angle": 45.0, "enable": true } }
{ "type": "select_model", "ts": 1781170003300, "payload": { "id": "openai:gpt-4o" } }
```

## 屏幕镜像帧（二进制）

每个二进制 WebSocket 帧 = 14 字节帧头 + 原始像素数据。

| 偏移 | 大小 | 字段 | 说明 |
|------|------|------|------|
| 0 | 4 | `magic` | 固定 `"EBF1"`（魔数 + 版本 1） |
| 4 | 2 | `width` | 宽（像素，小端） |
| 6 | 2 | `height` | 高（像素，小端） |
| 8 | 1 | `format` | 像素格式：`1`=RGB888，`2`=RGB565 |
| 9 | 1 | `flags` | 保留标志位（当前 0） |
| 10 | 4 | `seq` | 帧序号（小端） |
| 14 | … | `pixels` | `width × height × bytesPerPixel` 字节 |

ElectronBot 屏幕为 240×240。RGB888 一帧像素 = 240×240×3 = 172,800 字节。

## 素材管理 REST 接口（非 WebSocket）

屏幕表情素材（GIF / 图片）是二进制文件，上传与缩略图走普通 HTTP，与上面同一个监听端口：

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/materials` | `multipart/form-data` 上传单个文件：字段 `name`（情绪名，Unicode 字母/数字含中文与 `_` `-`，≤24 字；拒绝 `.` `/` `\` 等路径字符）+ 文件 `file`（`.gif` / `.png` / `.jpg`；或原始视频 `.mp4/...`，仅此情况走服务端 ffmpeg，可选）。落盘到 `emotions/` 并热重载，成功返回 `{"ok":true,"name":"..."}`；非法名/无效文件返回 `400`。上限 64 MiB |
| `POST` | `/api/material-frames` | `multipart/form-data` 上传**帧序列**（前端浏览器对视频抽帧的结果，无需 ffmpeg）：字段 `name` + `fps`（整数）+ 多个 `frame`（PNG/JPEG，按上传顺序为帧序，≤600 帧）。落盘为 `emotions/<情绪>/` 帧序列 + `clip.json`，热重载并广播 `materials` |
| `GET` | `/api/material-thumb?name=<情绪>` | 返回该情绪动画**首帧**的 240×240 PNG 缩略图；无此素材返回 `404` |

落盘形式（与 `display.LoadClips` 的识别规则一致）：GIF → `emotions/<情绪>.gif`（纯 Go 解码）；静态图片 → `emotions/<情绪>/0001.<ext>`。
删除/列表则走上面的 `material_delete` 命令与 `materials` 事件。

## 版本与演进

- 帧头魔数末位为版本号（当前 `1`）。不兼容变更时递增魔数版本。
- JSON 负载新增字段应保持向后兼容（旧端忽略未知字段）。
- 任何变更同时更新：`internal/protocol`、`web/src/protocol.ts`、本文件。
