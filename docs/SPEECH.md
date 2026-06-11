# 语音 sidecar 协议

主程序（纯 Go，无 cgo）不直接访问麦克风与扬声器，而是与一个本地 **语音 sidecar**
进程通过 WebSocket 通信。sidecar 负责音频设备 + 模型推理（推荐 sherpa-onnx 做
唤醒/VAD/ASR，Piper 做 TTS）。

Go 侧实现见 `internal/speech`：
- `Mock`：无音频，链路占位（默认）。
- `Sidecar`：连接到 `SPEECH_SIDECAR_URL` 指定的 WebSocket。

## 连接

```
SPEECH_SIDECAR_URL=ws://127.0.0.1:7800/speech go run ./cmd/electronstudio
```

主程序作为客户端拨号连接 sidecar 暴露的 WebSocket 端点。

## 消息（JSON 文本帧）

### sidecar → 主程序（上行语音输入）

| `type` | 字段 | 说明 |
|--------|------|------|
| `wake` | `keyword` | 命中唤醒词 |
| `vad`  | `speaking`(bool), `level`(0~1) | 语音活动 + 电平（驱动波形） |
| `asr`  | `text`, `final`(bool) | 识别结果，可多次中间态，`final=true` 为最终 |

```jsonc
{ "type": "wake", "keyword": "你好小电" }
{ "type": "vad",  "speaking": true, "level": 0.62 }
{ "type": "asr",  "text": "打开台灯", "final": true }
```

主程序对上行事件的处理：
- `wake` → 广播 `wake` + 语音状态切到 `listening`
- `vad`  → 广播 `vad`（前端波形）
- `asr` 且 `final` → 作为一次用户输入触发对话（与前端 `send_text` 同一路径）

### 主程序 → sidecar（下行合成请求）

| `type` | 字段 | 说明 |
|--------|------|------|
| `speak` | `text` | 合成并播放该文本 |
| `abort` | — | 打断当前播放 |

```jsonc
{ "type": "speak", "text": "好的，已为你打开" }
{ "type": "abort" }
```

## sidecar 实现建议

- **ASR / 唤醒 / VAD**：sherpa-onnx（中文 Paraformer / SenseVoice），自带麦克风采集；
  按上表把事件以 JSON 推给主程序。
- **TTS**：Piper（中文音色），收到 `speak` 后合成并直接播放到扬声器；`abort` 停止播放。
- sidecar 按平台预编译分发，置于 `sidecars/<平台>/`，不入库（见 `.gitignore`）。

## 状态上报

`Sidecar.Status()` 依据连接状态返回 ASR/TTS 是否在线，主程序据此填充前端
`status` 事件中的 `asr` / `tts` 字段。
