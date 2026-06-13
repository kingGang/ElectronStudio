#!/usr/bin/env bash
# 下载并归一化语音 sidecar 所需的模型（ASR=SenseVoice，VAD=Silero，TTS=VITS-zh 或 Piper）。
# 归一化后的路径与 config.example.json 一致。
#
# 用法：
#   bash download_models.sh                                # 默认：ASR+VAD+VITS-zh
#   TTS=piper bash download_models.sh                      # TTS 换 Piper（huayan-medium）
#   TTS=both  bash download_models.sh                      # 两种 TTS 都下
#   MIRROR=https://ghfast.top/ bash download_models.sh     # 国内 GitHub 加速（前缀式镜像）
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p models
cd models

MIRROR="${MIRROR:-}"     # GitHub 加速前缀；为空则直连
TTS="${TTS:-vits-zh}"    # vits-zh | piper | both

# 给 GitHub 直链加镜像前缀（MIRROR 为空则原样）。
mirrored() { if [ -n "$MIRROR" ]; then echo "${MIRROR%/}/$1"; else echo "$1"; fi; }
BASE="https://github.com/k2-fsa/sherpa-onnx/releases/download"
ASR_URL="$BASE/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17.tar.bz2"
VAD_URL="$BASE/asr-models/silero_vad.onnx"
TTS_ZH_URL="$BASE/tts-models/vits-zh-hf-fanchen-C.tar.bz2"
TTS_PIPER_URL="$BASE/tts-models/vits-piper-zh_CN-huayan-medium.tar.bz2"

echo "==> 下载 ASR：SenseVoice"
curl -L -o sense-voice.tar.bz2 "$(mirrored "$ASR_URL")"
tar xf sense-voice.tar.bz2
rm -rf sense-voice && mv sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17 sense-voice
rm -f sense-voice.tar.bz2

echo "==> 下载 VAD：Silero"
curl -L -o silero_vad.onnx "$(mirrored "$VAD_URL")"

if [ "$TTS" = "vits-zh" ] || [ "$TTS" = "both" ]; then
  echo "==> 下载 TTS：VITS-zh (fanchen-C)"
  curl -L -o vits-zh.tar.bz2 "$(mirrored "$TTS_ZH_URL")"
  tar xf vits-zh.tar.bz2
  rm -rf vits-zh && mv vits-zh-hf-fanchen-C vits-zh
  mv vits-zh/vits-zh-hf-fanchen-C.onnx vits-zh/model.onnx
  rm -f vits-zh.tar.bz2
fi

if [ "$TTS" = "piper" ] || [ "$TTS" = "both" ]; then
  echo "==> 下载 TTS：Piper zh_CN huayan-medium"
  curl -L -o vits-piper.tar.bz2 "$(mirrored "$TTS_PIPER_URL")"
  tar xf vits-piper.tar.bz2
  rm -rf vits-piper && mv vits-piper-zh_CN-huayan-medium vits-piper
  mv vits-piper/zh_CN-huayan-medium.onnx vits-piper/model.onnx
  rm -f vits-piper.tar.bz2
fi

echo "==> 完成。模型位于 $(pwd)"
echo "    现在可：cp ../config.example.json ../config.json && python ../sidecar.py -c ../config.json"
if [ "$TTS" = "piper" ]; then
  echo ""
  echo "    Piper：把 config.json 的 tts 段改为指向 models/vits-piper 并设 data_dir："
  echo '      "tts": { "model":"models/vits-piper/model.onnx", "tokens":"models/vits-piper/tokens.txt",'
  echo '               "data_dir":"models/vits-piper/espeak-ng-data", "lexicon":"", "dict_dir":"", "speaker_id":0, "speed":1.0 }'
fi
