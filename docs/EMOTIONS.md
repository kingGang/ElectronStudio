# 表情画面

机器人屏幕上的"脸"由主机实时生成并推送（设备屏只写，见 docs/ELECTRONBOT.md）。
两种来源，由 `internal/display.Compositor` 自动选择：

## A. 程序实时动画脸（默认，无需任何素材）

`internal/display.EmotionSource`：纯 Go 渲染极简表情（眼睛 + 嘴），并**实时动画**：
- **AI 决定情绪**：大模型工具 `set_emotion` / 情绪事件 → 切换表情。
- **口型同步**：TTS 播放期间嘴随节奏张合（`SetSpeaking`）。
- **眨眼**：约每 3 秒眨一次。

由 `device.Driver` 30fps 驱动；仅在画面变化时推帧，省带宽。开箱即用，可在树莓派跑。

## B. 离线（AI）生成的精致表情素材（可选）

把每个情绪做成一段 **240×240 的帧序列**，运行时循环播放；有素材的情绪用素材，没有的回退到程序脸。

### 放置位置

与 `config.json` 同目录下的 `emotions/`：

```
emotions/
  happy/    0001.png 0002.png ...   (240×240)
  sad/      0001.png ...
  thinking/ ...
```

文件名按字典序即为帧序。支持 PNG / JPG，**尺寸必须 240×240**。

### 怎么做素材

- **AI 图像/视频生成**：用图像模型为每个情绪生成一张/一段表情（如"赛博青绿色机器人笑脸，圆形，黑底"），
  视频模型或多张关键帧做成循环动画。
- **从 GIF/MP4 转帧**（ffmpeg）：

  ```bash
  ffmpeg -i happy.gif  -vf "scale=240:240" emotions/happy/%04d.png
  ffmpeg -i happy.mp4  -vf "fps=15,scale=240:240" emotions/happy/%04d.png
  ```

- **空闲后台按需生成**（进阶）：可写一个生成 sidecar，在空闲时为缺失情绪生成素材并落盘，
  下次自动加载——这也是接入"真·实时逐帧生成（GPU 服务器）"的同一 `Source` 接口位置。

素材较大，已在 `.gitignore` 排除，不入库。
