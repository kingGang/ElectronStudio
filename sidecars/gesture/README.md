# 手势 sidecar（参考实现）

用 [MediaPipe](https://developers.google.com/mediapipe) 的 GestureRecognizer 实时识别手势，
经 WebSocket 把手势事件推给纯 Go 主程序。架构与 `sidecars/speech` 对称。

> 说明：ElectronBot 虽有硬件手势传感器，但其数据不在 USB 自定义协议里（只有图像 + 32 字节舵机反馈），
> 取不到。所以手势识别走**视觉**（摄像头）。

## 快速开始

```bash
python -m venv .venv && source .venv/bin/activate   # Windows: .venv\Scripts\activate
pip install -r requirements.txt

# 下载手势模型
curl -L -o gesture_recognizer.task \
  https://storage.googleapis.com/mediapipe-models/gesture_recognizer/gesture_recognizer/float16/1/gesture_recognizer.task

python sidecar.py --camera 0 --ws-port 7810
```

让 Go 主程序对接（在 `config.json` 配置）：

```jsonc
"gesture": { "sidecar_url": "ws://127.0.0.1:7810" }
```

## 手势 → 行为（主程序默认映射）

| 手势（sidecar 输出） | MediaPipe 类别 | 机器人行为 |
|----------------------|----------------|-----------|
| `thumbs_up` | Thumb_Up | 开心 + 点头 |
| `victory` | Victory | 庆祝（cheer） |
| `open_palm` | Open_Palm | 停止当前动作/语音 |
| `fist` | Closed_Fist | 摇头 |
| `wave` | （动作类，需自定义） | 看一眼打招呼 |

> `wave`（挥手）是动作手势，MediaPipe 内置集没有；如需可在 sidecar 里按手掌横向移动自行检测后输出 `wave`。

## 注意

- 摄像头是独占设备：若同时开启主程序的"摄像头上屏"(ffmpeg)，会与本 sidecar 争抢同一摄像头。
- MediaPipe API 在不同版本略有差异，本实现按 0.10.x 编写。
- 模型文件已在 `.gitignore` 排除，不入库。
