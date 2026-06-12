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
| `status` | `StatusEvent` | 各子系统（USB / ASR / TTS / LLM）状态快照，连接建立或状态变化时下发 |
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
| `materials` | `MaterialsEvent` | 屏幕表情素材列表（上传/删除后广播，连接时推送一次） |
| `log` | `LogEvent` | 调试日志（可选） |

### 示例

```jsonc
// 状态快照
{ "type": "status", "ts": 1781170000000, "payload": {
  "robot": { "connected": true, "vid": 4097, "pid": 32803, "fps": 30 },
  "asr":   { "running": true,  "detail": "SenseVoice-zh" },
  "tts":   { "running": true,  "detail": "piper zh_CN-huayan" },
  "llm":   { "active": "ollama:qwen2.5", "available": [
              { "id": "ollama:qwen2.5", "name": "Qwen2.5", "provider": "ollama" },
              { "id": "openai:gpt-4o",  "name": "GPT-4o",  "provider": "openai" } ] }
}}

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
| `POST` | `/api/materials` | `multipart/form-data` 上传：字段 `name`（情绪名，仅 `[a-z0-9_-]`，≤40）+ 文件 `file`（`.gif` / `.png` / `.jpg` / 视频 `.mp4/.webm/.mov/.mkv/.avi/.m4v`）。落盘到 `emotions/` 并热重载，成功返回 `{"ok":true,"name":"..."}`；非法名/无效文件返回 `400`。上限 64 MiB。视频经 ffmpeg 抽帧（服务器需装 ffmpeg，否则该类型返回 400 提示） |
| `GET` | `/api/material-thumb?name=<情绪>` | 返回该情绪动画**首帧**的 240×240 PNG 缩略图；无此素材返回 `404` |

落盘形式（与 `display.LoadClips` 的识别规则一致）：GIF → `emotions/<情绪>.gif`（纯 Go 解码）；静态图片 → `emotions/<情绪>/0001.<ext>`。
删除/列表则走上面的 `material_delete` 命令与 `materials` 事件。

## 版本与演进

- 帧头魔数末位为版本号（当前 `1`）。不兼容变更时递增魔数版本。
- JSON 负载新增字段应保持向后兼容（旧端忽略未知字段）。
- 任何变更同时更新：`internal/protocol`、`web/src/protocol.ts`、本文件。
