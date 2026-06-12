# 下载并归一化语音 sidecar 所需的模型（Windows）。
# 归一化后的路径与 config.example.json 一致。
# 用法：pwsh ./download_models.ps1
$ErrorActionPreference = 'Stop'

Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force models | Out-Null
Set-Location models

$asr = 'https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17.tar.bz2'
$vad = 'https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx'
$tts = 'https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-zh-hf-fanchen-C.tar.bz2'

Write-Host '==> 下载 ASR：SenseVoice'
Invoke-WebRequest -Uri $asr -OutFile sense-voice.tar.bz2
tar xf sense-voice.tar.bz2   # Windows 10+ 自带 bsdtar，支持 bz2
if (Test-Path sense-voice) { Remove-Item -Recurse -Force sense-voice }
Rename-Item sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17 sense-voice
Remove-Item sense-voice.tar.bz2

Write-Host '==> 下载 VAD：Silero'
Invoke-WebRequest -Uri $vad -OutFile silero_vad.onnx

Write-Host '==> 下载 TTS：VITS-zh (fanchen-C)'
Invoke-WebRequest -Uri $tts -OutFile vits-zh.tar.bz2
tar xf vits-zh.tar.bz2
if (Test-Path vits-zh) { Remove-Item -Recurse -Force vits-zh }
Rename-Item vits-zh-hf-fanchen-C vits-zh
Rename-Item vits-zh/vits-zh-hf-fanchen-C.onnx vits-zh/model.onnx
Remove-Item vits-zh.tar.bz2

Write-Host "==> 完成。模型位于 $(Get-Location)"
Write-Host '    现在可：copy ..\config.example.json ..\config.json; python ..\sidecar.py -c ..\config.json'
