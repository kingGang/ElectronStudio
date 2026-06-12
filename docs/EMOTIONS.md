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

### 方式零：在界面里上传（推荐，无需脚本/外部工具）

打开前端顶栏的 **「素材」** 页：填情绪名（支持中文/字母/数字/`-`/`_`，如 `开心` / `happy` / `dance`，≤24 字）、选一段 **GIF / 视频**（或一张静态图片）点上传即可。
后端用**纯 Go**（标准库 `image/gif`，无 cgo、无 ffmpeg/TexturePacker）解码 GIF，按其逐帧处置方式正确合成整帧、帧率自动取自 GIF，
落盘到 `emotions/` 并**立即热重载**——设备屏与 UI 镜像同步生效。每张卡片可「▶ 预览」（让机器人切到该情绪）或「✕ 删除」（回退程序脸）。

- **视频**（`.mp4/.webm/.mov/.mkv/.avi/.m4v`）：受「主程序无 cgo」约束，视频解码交给外部 **ffmpeg** 子进程
  （与摄像头采集同一依赖；服务器需装有 ffmpeg）。上传后按约 **20fps** 重采样、等比缩放并**居中裁剪**到 240×240，
  抽帧落盘为帧序列；抽帧在锁外完成，仅“替换+热重载”持锁，且**抽帧成功后才动旧素材**（失败不丢数据）。
- 上传走 HTTP `POST /api/materials`，列表/删除走 WebSocket（`materials` 事件 / `material_delete` 命令）。
- 落盘形式：GIF → `emotions/<情绪>.gif`；视频 → `emotions/<情绪>/`（帧序列 + `clip.json` 记 fps）；静态图片 → `emotions/<情绪>/0001.<ext>`。
- 下面「方式一/二」是给已有帧序列或图集的进阶用户/批量场景；日常加表情用界面上传即可。

> 已知行为（圆屏取舍，非缺陷）：① 屏幕是 **240×240 圆形黑底**，GIF 的透明/背景区域一律渲染为黑；
> ② 播放按**整段平均帧率**循环，逐帧变速的 GIF（如首帧长停顿）会按平均速率播放；
> ③ 上传有大小/帧数上限（单边 ≤4096、≤600 帧、≤32MiB），用于防御构造的「解码炸弹」。
> 校验在落盘前完成：无效文件会被拒绝且**不会覆盖**已有同名素材。

### 放置位置（手动放素材：两种方式，放在与 `config.json` 同目录的 `emotions/`）

**方式一：一帧一文件（目录）**

```
emotions/
  happy/    0001.png 0002.png ...
  sad/      0001.png ...
```

文件名按字典序即为帧序。支持 PNG / JPG；任意尺寸（非 240×240 会自动缩放）。

**方式二：精灵图集（推荐）**

`emotions/<情绪>.json` 为入口（情绪名 = json 文件名），自动识别两种图集格式：

**(a) TexturePacker（推荐）**

TexturePacker 导出选 **Data Format = `JSON (Array)`**（数组有序 = 帧序），一个情绪一套：

```
emotions/
  happy.png     # 图集（TexturePacker 输出的 sheet，名字由 meta.image 指定）
  happy.json    # TexturePacker 的 JSON(Array)
```

- 自动处理 `rotated`（图集里旋转打包）与 `trimmed`（去透明边）：每帧复原为正立整帧再缩放到 240×240。
- 帧率：TexturePacker 不导出 fps，默认 **15**；要改就在 JSON 里加一个 `"fps": 12`（顶层或 `meta` 内均可）。
- 图集 PNG 路径取自 `meta.image`，找不到则回退同名 `<情绪>.png`。

> 批量从 GIF 生成：`scripts/pack_gifs.{sh,ps1}`（GIF→抽帧→TexturePacker 打包，自动取 fps）。见 `scripts/README.md`。

**(b) 自有等距网格**

`emotions/<情绪>.png` + `emotions/<情绪>.json`：

```json
{ "frame_width": 240, "frame_height": 240, "frames": 8, "fps": 12 }
```

按等距网格切（左→右、上→下），每帧缩放到 240×240。例如 1920×240 = 8 帧横向条带。

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

## C. 摄像头画面（屏幕显示实时画面）

ElectronBot 的摄像头是板载 USB Hub 上的标准 UVC 设备，主机当普通 webcam 读取（见 docs/ELECTRONBOT.md）。
受"主程序无 cgo"约束，采集交给外部 **ffmpeg** 子进程：抓摄像头、缩放 240×240、输出 rgb24 裸帧，
Go 侧按帧长读取后作为 `display.Source`，与表情走**同一条推屏管线**——设备屏与 UI 镜像照样同步。

### 配置（`config.json`）

```jsonc
"camera": {
  "enabled": true,
  "ffmpeg": "ffmpeg",          // 可执行路径
  "input_format": "v4l2",      // Linux=v4l2 / Windows=dshow / macOS=avfoundation
  "input": "/dev/video0"       // 设备规格
}
```

各平台 `input_format` / `input` 示例：

| 平台 | input_format | input 示例 |
|------|--------------|-----------|
| 树莓派 / Linux | `v4l2` | `/dev/video0` |
| Windows | `dshow` | `video=Integrated Camera`（名字用 `ffmpeg -list_devices true -f dshow -i dummy` 查） |
| macOS | `avfoundation` | `0`（设备索引） |

### 用法

启用后，前端首页机器人区出现 **📷 摄像头** 按钮，点击在"表情脸 / 摄像头画面"间切换；
开启时屏幕与 UI 镜像显示实时摄像头画面，关闭时切回表情。也可由命令 `camera {enable}` 控制。

> 这同一个 `display.Source` 接口，也是将来接"AI 实时生成帧（GPU 服务器）"的位置。

### AI 看摄像头（视觉）

配置了摄像头后，会自动注册一个 **`look` 工具**：支持工具调用的大模型可以"看一眼"——
主机抓当前摄像头帧 → 编码 JPEG → 交给**视觉模型**描述/回答 → 结果回到对话。

- 需要把一个**支持视觉的模型**设为生效（如 `gpt-4o`，或本地 Ollama 的 `llava` / `qwen2-vl`）。
- 用户说"你看到了什么 / 帮我看看桌上有什么"，模型会调用 `look`，机器人就能"看着说"。
- `look` 会在需要时临时启动摄像头采集抓帧（不改变屏幕当前显示）。
