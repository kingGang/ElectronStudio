# ElectronStudio

面向 [ElectronBot](https://github.com/peng-zhihui/ElectronBot) 桌面机器人的跨平台语音助手，使用 **纯 Go**（`CGO_ENABLED=0`，可交叉编译到 macOS / Windows / 树莓派）编写后端，Web 前端可重构。

## 设计目标

| # | 目标 | 方案 |
|---|------|------|
| 1 | 不依赖 C 工具链 | 主程序纯 Go 交叉编译；原生能力（USB / 语音）通过 sidecar 二进制或 purego 运行时加载 |
| 2 | 多平台运行 | macOS / Windows / 树莓派 |
| 3 | 界面可重构 | Go 后端 + Web 前端（浏览器 / webview），深色科技风 |
| 4 | 保留动作编排 | 移植 ElectronBot 的 6 轴关键帧动作序列 |
| 5 | 本地语音 | sherpa-onnx（唤醒 + VAD + ASR）+ Piper（TTS）以 sidecar 形式本地运行 |
| 6 | 多大模型 | LLM Provider 抽象，配置文件可挂任意多个（Ollama / OpenAI 兼容 / Anthropic …） |

## 硬件

ElectronBot 通过 **USB bulk** 通信（VID `0x1001` / PID `0x8023`）：上位机下发 240×240 RGB888 画面 + 6 轴舵机角度，读回真实角度。屏幕、舵机的 SPI/I2C 驱动都在机器人 STM32 固件内部完成。

## 目录结构

```
cmd/electronstudio/      程序入口
internal/
  protocol/              ★ 前后端 WebSocket 消息协议（单一事实来源）
  server/                HTTP + WebSocket 服务
  fsm/                   对话状态机
  choreography/          动作编排：情绪 → 6 轴角度序列
  llm/                   LLM Provider 抽象与路由
  speech/                唤醒 / VAD / ASR / TTS 协调（对接 sidecar）
  robot/                 机器人传输层接口
    electronbot/         purego + libusb 实现
web/                     前端单页应用（Vue3 + Vite，go:embed 打包）
sidecars/                各平台预编译的 sherpa-onnx / Piper 二进制
docs/                    设计与协议文档
```

## 构建

```bash
# 主程序（无 C 工具链）
CGO_ENABLED=0 go build ./cmd/electronstudio

# 交叉编译示例（树莓派 64 位）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/electronstudio
```

## 一键启动（推荐）

跨平台启动脚本：自动建 Python 虚拟环境、装 sidecar 依赖、下载语音模型、（可选）编译，
再起语音 sidecar + 主程序，缺啥补啥、已就绪则秒起。Ctrl+C 退出时一并关闭 sidecar。

```bash
# Linux / macOS / 树莓派
./scripts/run.sh                       # 监听 :8080，含语音 sidecar
./scripts/run.sh --mirror https://ghfast.top/   # 国内加速下模型
./scripts/run.sh --no-sidecar          # 只跑文本链路（不装 Python/不下模型）
./scripts/run.sh --build --addr :8099  # 编译成 bin/electronstudio 再跑
./scripts/run.sh --help                # 全部参数
```

```powershell
# Windows（或 scripts\run.bat 双击）
pwsh scripts/run.ps1
pwsh scripts/run.ps1 -Mirror https://ghfast.top/ -Tts piper
```

首次运行会下载约数百 MB 的语音模型（SenseVoice / Silero / VITS-zh），之后自动跳过。
脚本不会改动你已有的 `config.json`（含密钥）；如需挂接大模型/QQ 音乐等，按下文手动配置。

## 运行（最小入口）

也可直接用 Go 命令。当前入口使用 **Mock 机器人 + 本地 Echo 模型**，无需真机 / 联网 / C 工具链即可启动：

```bash
go run ./cmd/electronstudio -addr :8080
# 连接 ws://localhost:8080/ws 即可看到事件流动
```

挂接真实大模型（纯 HTTP，无 cgo；本地 Ollama 或任意 OpenAI 兼容服务）：

```bash
# 例：本地 Ollama
OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_MODEL=qwen2.5 go run ./cmd/electronstudio
# 例：OpenAI
OPENAI_BASE_URL=https://api.openai.com/v1 OPENAI_API_KEY=sk-xxx OPENAI_MODEL=gpt-4o go run ./cmd/electronstudio
```

## 协议

前后端通信契约见 [`docs/PROTOCOL.md`](docs/PROTOCOL.md)，Go 侧实现于 `internal/protocol`，前端类型镜像于 `web/src/protocol.ts`。

## 进度

- [x] `internal/protocol` 消息契约（+ TS 镜像 + 文档）
- [x] `internal/server` WebSocket 服务（连接管理 / 广播 / 心跳）
- [x] `internal/robot` 传输接口 + Mock 实现
- [x] `internal/choreography` 动作编排引擎（关键帧插值）
- [x] `internal/llm` 多模型路由（Echo / OpenAI 兼容）
- [x] `cmd/electronstudio` 最小可运行入口
- [x] `internal/speech` 语音 sidecar 对接（唤醒 / VAD / ASR / TTS）
- [x] `web` 前端单页应用（对话 / 动作编排 / 设置，深色科技风，go:embed 内嵌）
- [x] `internal/tools` 工具调用 + 设备控制（function-calling）
- [x] `internal/config` 配置持久化（设置页可增删模型并落盘）
- [x] `internal/robot/electronbot` 真机 USB 传输（purego + libusb，无 cgo）

真机接入见 [`docs/ELECTRONBOT.md`](docs/ELECTRONBOT.md)。默认 `robot: auto`：装好 libusb、接上 ElectronBot，启动即自动连接；无真机则回退 Mock。
