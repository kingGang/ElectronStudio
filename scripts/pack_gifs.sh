#!/usr/bin/env bash
# 批量把 GIF 打包成各情绪精灵图集（TexturePacker JSON-Array），供 ElectronStudio 表情使用。
#
# 用法：  scripts/pack_gifs.sh <GIF目录> <输出emotions目录>
# 例如：  scripts/pack_gifs.sh ./gifs ./emotions
#   gifs/happy.gif → emotions/happy.png + emotions/happy.json（情绪名 = GIF 文件名）
#
# 依赖：ffmpeg + ffprobe（抽帧/取帧率） + TexturePacker（命令行）
set -euo pipefail

SRC="${1:-./gifs}"
OUT="${2:-./emotions}"
SIZE=240          # 每帧缩放到 240×240（圆屏尺寸）
MAXSIZE=4096      # 图集最大边

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少依赖：$1"; exit 1; }; }
need ffmpeg; need ffprobe; need TexturePacker

mkdir -p "$OUT"

# 把 fps 写入 TexturePacker 导出的 json（顶层 "fps"，ElectronStudio 加载时读取）。
inject_fps() {
  local json="$1" fps="$2"
  if command -v jq >/dev/null 2>&1; then
    local t; t="$(mktemp)"; jq ". + {fps: $fps}" "$json" >"$t" && mv "$t" "$json"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$json" "$fps" <<'PY'
import json,sys
p,fps=sys.argv[1],int(sys.argv[2])
d=json.load(open(p,encoding="utf-8")); d["fps"]=fps
json.dump(d,open(p,"w",encoding="utf-8"),ensure_ascii=False,indent=2)
PY
  else
    echo "    （无 jq/python3，未写 fps；ElectronStudio 加载时默认 15）"
  fi
}

shopt -s nullglob
found=0
for gif in "$SRC"/*.gif "$SRC"/*.GIF; do
  found=1
  name="$(basename "${gif%.*}")"
  echo "==> $name"

  tmp="$(mktemp -d)"
  # 抽帧，缩放到 SIZE×SIZE（lanczos 高质量缩放，保留透明）
  ffmpeg -y -v error -i "$gif" -vf "scale=${SIZE}:${SIZE}:flags=lanczos" "$tmp/%04d.png"
  nframes="$(find "$tmp" -name '*.png' | wc -l | tr -d ' ')"

  # 取 GIF 平均帧率（形如 "10/1"）→ 整数 fps
  rate="$(ffprobe -v error -select_streams v:0 -show_entries stream=avg_frame_rate -of csv=p=0 "$gif" 2>/dev/null || true)"
  fps=15
  if [[ "$rate" =~ ^([0-9]+)/([0-9]+)$ ]] && [ "${BASH_REMATCH[2]}" -gt 0 ]; then
    fps=$(( (10#${BASH_REMATCH[1]} + 10#${BASH_REMATCH[2]} / 2) / 10#${BASH_REMATCH[2]} ))
  elif [[ "$rate" =~ ^([0-9]+)$ ]]; then
    fps=$(( 10#${BASH_REMATCH[1]} ))
  fi
  [ "$fps" -lt 1 ] && fps=15

  # TexturePacker 打包为 JSON (Array)；输入文件夹放最后（位置参数）。
  TexturePacker \
    --format json-array \
    --sheet "$OUT/$name.png" \
    --data "$OUT/$name.json" \
    --trim-mode Trim \
    --disable-rotation \
    --max-size "$MAXSIZE" \
    --quiet \
    "$tmp" || { echo "    TexturePacker 失败：$name"; rm -rf "$tmp"; continue; }

  inject_fps "$OUT/$name.json" "$fps"
  rm -rf "$tmp"
  echo "    -> $OUT/$name.png + $name.json（$nframes 帧, fps=$fps）"
done

[ "$found" = 1 ] || { echo "在 $SRC 没找到 *.gif"; exit 1; }
echo "完成。把 $OUT 放到与 config.json 同目录即可。"
