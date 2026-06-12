#!/usr/bin/env bash
# 下载并归一化语音 sidecar 所需的模型（ASR=SenseVoice，VAD=Silero，TTS=VITS-zh）。
# 归一化后的路径与 config.example.json 一致，下载完成即可直接运行。
#
# 用法：bash download_models.sh
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p models
cd models

ASR_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17.tar.bz2"
VAD_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx"
TTS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-zh-hf-fanchen-C.tar.bz2"

echo "==> 下载 ASR：SenseVoice"
curl -L -o sense-voice.tar.bz2 "$ASR_URL"
tar xf sense-voice.tar.bz2
rm -rf sense-voice && mv sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17 sense-voice
rm -f sense-voice.tar.bz2

echo "==> 下载 VAD：Silero"
curl -L -o silero_vad.onnx "$VAD_URL"

echo "==> 下载 TTS：VITS-zh (fanchen-C)"
curl -L -o vits-zh.tar.bz2 "$TTS_URL"
tar xf vits-zh.tar.bz2
rm -rf vits-zh && mv vits-zh-hf-fanchen-C vits-zh
mv vits-zh/vits-zh-hf-fanchen-C.onnx vits-zh/model.onnx
rm -f vits-zh.tar.bz2

echo "==> 完成。模型位于 $(pwd)"
echo "    现在可：cp ../config.example.json ../config.json && python ../sidecar.py -c ../config.json"
