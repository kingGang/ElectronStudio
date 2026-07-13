#!/usr/bin/env bash
# ElectronStudio 一键启动（Linux / macOS / 树莓派）：
#   装依赖 + 下模型 + 构建 + 起语音 sidecar + 跑主程序，缺啥补啥、已就绪则秒起。
#   Windows 用 scripts/run.ps1。
#
# 用法：
#   ./scripts/run.sh                       # setup(按需) + go run，监听 :8080
#   ./scripts/run.sh --build               # 先编译 bin/electronstudio 再跑
#   ./scripts/run.sh --addr :8099          # 换监听地址
#   ./scripts/run.sh --tts piper           # 语音模型 TTS 改用 Piper（默认 vits-zh）
#   ./scripts/run.sh --mirror https://ghfast.top/   # GitHub 加速（国内下模型快）
#   ./scripts/run.sh --no-sidecar          # 只跑主程序(纯文本链路)，不起语音 sidecar
#   ./scripts/run.sh --skip-models --skip-deps   # 已装好时跳过下载/装包
#   ./scripts/run.sh --setup-only          # 只准备环境，不启动
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SPEECH="$ROOT/sidecars/speech"
cd "$ROOT"

ADDR=":8080"
MIRROR="${MIRROR:-}"
TTS="${TTS:-vits-zh}"
BUILD=0; NO_SIDECAR=0; SKIP_MODELS=0; SKIP_DEPS=0; SETUP_ONLY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --addr) ADDR="$2"; shift 2;;
    --mirror) MIRROR="$2"; shift 2;;
    --tts) TTS="$2"; shift 2;;
    --build) BUILD=1; shift;;
    --no-sidecar) NO_SIDECAR=1; shift;;
    --skip-models) SKIP_MODELS=1; shift;;
    --skip-deps) SKIP_DEPS=1; shift;;
    --setup-only) SETUP_ONLY=1; shift;;
    -h|--help) grep '^#' "$0" | sed 's/^#\{1,\} \{0,1\}//'; exit 0;;
    *) echo "未知参数: $1（用 --help 看用法）" >&2; exit 2;;
  esac
done

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

# 1) 前置检查
command -v go >/dev/null 2>&1 || die "未找到 go，请先安装 Go (>=1.23)：https://go.dev/dl/"

PY=""
if [ "$NO_SIDECAR" -eq 0 ]; then
  for c in python3 python; do command -v "$c" >/dev/null 2>&1 && { PY="$c"; break; }; done
  [ -n "$PY" ] || die "未找到 python3（语音 sidecar 需要）。装好后重试，或加 --no-sidecar 只跑文本链路。"
fi

SIDECAR_URL="ws://127.0.0.1:7800"
VPY=""

# 2) 语音 sidecar：虚拟环境 + 依赖
if [ "$NO_SIDECAR" -eq 0 ]; then
  VENV="$SPEECH/.venv"
  if [ ! -d "$VENV" ]; then
    log "创建 Python 虚拟环境 $VENV"
    "$PY" -m venv "$VENV"
  fi
  if [ -x "$VENV/bin/python" ]; then VPY="$VENV/bin/python"; else VPY="$VENV/Scripts/python.exe"; fi

  if [ "$SKIP_DEPS" -eq 0 ]; then
    log "安装语音 sidecar 依赖 (pip install -r requirements.txt)"
    "$VPY" -m pip install --upgrade pip >/dev/null
    "$VPY" -m pip install -r "$SPEECH/requirements.txt"
  fi

  # 3) 模型：缺则下载（已有 sense-voice/model.onnx 视为就绪）
  if [ "$SKIP_MODELS" -eq 0 ] && [ ! -f "$SPEECH/models/sense-voice/model.onnx" ]; then
    log "下载语音模型 (ASR=SenseVoice / VAD=Silero / TTS=$TTS)"
    MIRROR="$MIRROR" TTS="$TTS" bash "$SPEECH/download_models.sh"
  else
    log "语音模型已存在，跳过下载"
  fi

  # 4) sidecar 配置
  if [ ! -f "$SPEECH/config.json" ]; then
    log "生成 sidecar 配置 config.json（复制自 config.example.json）"
    cp "$SPEECH/config.example.json" "$SPEECH/config.json"
  fi
fi

# 5) 构建（可选）
# macOS 上启用 cgo：摄像头到屏用进程内 AVFoundation 采集(需 cgo)，根治"摄像头与主控共用板载 Hub
# 导致的屏幕卡死"。其他平台保持 CGO_ENABLED=0 纯 Go（无 camcap 子进程时自动回落，交叉编译不受影响）。
CGOFLAG=0
[ "$(uname)" = "Darwin" ] && CGOFLAG=1
BIN=""
if [ "$BUILD" -eq 1 ]; then
  log "编译主程序 -> bin/electronstudio (CGO_ENABLED=$CGOFLAG)"
  mkdir -p "$ROOT/bin"
  BIN="$ROOT/bin/electronstudio"
  CGO_ENABLED=$CGOFLAG go build -o "$BIN" ./cmd/electronstudio
fi

if [ "$SETUP_ONLY" -eq 1 ]; then log "准备完成（--setup-only）。"; exit 0; fi

# 5.5) 国内 API 不走代理。Go 默认 ProxyFromEnvironment，程序继承 shell 的 HTTP(S)_PROXY 后，
# 连国内的模型/音乐服务也会被塞进代理绕一圈——实测 MiniMax 经代理 3.0s、直连 1.3s（慢 2.3 倍），
# 长回答时更容易拖到超时。这里只给本程序追加 no_proxy 白名单，不动系统/shell 的代理设置。
# 需要经代理访问的境外服务（OpenAI 等）不受影响，照常走代理。
BYPASS="localhost,127.0.0.1,::1,api.minimaxi.com,.minimaxi.com,.qq.com,.kuwo.cn"
export no_proxy="${no_proxy:+$no_proxy,}$BYPASS"
export NO_PROXY="$no_proxy"
[ -n "${HTTP_PROXY:-$http_proxy}" ] && log "代理白名单(不走代理): $BYPASS"

# 6) 启动 sidecar（后台）+ 主程序；退出时清理 sidecar
SIDE_PID=""
cleanup() { [ -n "$SIDE_PID" ] && kill "$SIDE_PID" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

if [ "$NO_SIDECAR" -eq 0 ]; then
  log "启动语音 sidecar ($SIDECAR_URL)"
  ( cd "$SPEECH" && exec "$VPY" sidecar.py -c config.json ) &
  SIDE_PID=$!
  export SPEECH_SIDECAR_URL="$SIDECAR_URL"
  ready=0
  for _ in $(seq 1 30); do
    kill -0 "$SIDE_PID" 2>/dev/null || die "sidecar 启动失败，见上方日志"
    if (exec 3<>/dev/tcp/127.0.0.1/7800) 2>/dev/null; then exec 3>&- 3<&-; ready=1; break; fi
    sleep 0.5
  done
  [ "$ready" -eq 1 ] && log "sidecar 已就绪" || log "sidecar 未及时就绪，继续启动（主程序会自动重连）"
fi

log "启动主程序 (addr=$ADDR) -> http://localhost${ADDR}"
if [ -n "$BIN" ]; then
  "$BIN" -addr "$ADDR"
else
  CGO_ENABLED=$CGOFLAG go run ./cmd/electronstudio -addr "$ADDR"
fi
