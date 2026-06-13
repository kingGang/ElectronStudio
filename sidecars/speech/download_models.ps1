# 下载并归一化语音 sidecar 所需的模型（Windows）。归一化后的路径与 config.example.json 一致。
#
# 用法：
#   pwsh ./download_models.ps1                                  # 默认：ASR+VAD+VITS-zh
#   pwsh ./download_models.ps1 -Tts piper                       # TTS 换 Piper（huayan-medium）
#   pwsh ./download_models.ps1 -Tts both                        # 两种 TTS 都下
#   pwsh ./download_models.ps1 -Mirror https://ghfast.top/      # 国内 GitHub 加速（前缀式镜像）
param(
  [string]$Mirror = '',                            # GitHub 加速前缀；国内可传 https://ghfast.top/
  [ValidateSet('vits-zh', 'piper', 'both')]
  [string]$Tts = 'vits-zh'                          # TTS 音色
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'           # 关进度条：IWR 下载大文件会快很多

Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force models | Out-Null
Set-Location models

# 给 GitHub 直链加镜像前缀（$Mirror 为空则原样直连）。
function Mirrored([string]$u) { if ($Mirror) { return ($Mirror.TrimEnd('/') + '/' + $u) } return $u }
$base     = 'https://github.com/k2-fsa/sherpa-onnx/releases/download'
$asr      = "$base/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17.tar.bz2"
$vad      = "$base/asr-models/silero_vad.onnx"
$ttsZh    = "$base/tts-models/vits-zh-hf-fanchen-C.tar.bz2"
$ttsPiper = "$base/tts-models/vits-piper-zh_CN-huayan-medium.tar.bz2"

Write-Host '==> 下载 ASR：SenseVoice'
Invoke-WebRequest -Uri (Mirrored $asr) -OutFile sense-voice.tar.bz2
tar xf sense-voice.tar.bz2   # Windows 10+ 自带 bsdtar，支持 bz2
if (Test-Path sense-voice) { Remove-Item -Recurse -Force sense-voice }
Rename-Item sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17 sense-voice
Remove-Item sense-voice.tar.bz2

Write-Host '==> 下载 VAD：Silero'
Invoke-WebRequest -Uri (Mirrored $vad) -OutFile silero_vad.onnx

if ($Tts -eq 'vits-zh' -or $Tts -eq 'both') {
  Write-Host '==> 下载 TTS：VITS-zh (fanchen-C)'
  Invoke-WebRequest -Uri (Mirrored $ttsZh) -OutFile vits-zh.tar.bz2
  tar xf vits-zh.tar.bz2
  if (Test-Path vits-zh) { Remove-Item -Recurse -Force vits-zh }
  Rename-Item vits-zh-hf-fanchen-C vits-zh
  Rename-Item vits-zh/vits-zh-hf-fanchen-C.onnx model.onnx   # -NewName 只能是叶名，不能带路径
  Remove-Item vits-zh.tar.bz2
}
if ($Tts -eq 'piper' -or $Tts -eq 'both') {
  Write-Host '==> 下载 TTS：Piper zh_CN huayan-medium'
  Invoke-WebRequest -Uri (Mirrored $ttsPiper) -OutFile vits-piper.tar.bz2
  tar xf vits-piper.tar.bz2
  if (Test-Path vits-piper) { Remove-Item -Recurse -Force vits-piper }
  Rename-Item vits-piper-zh_CN-huayan-medium vits-piper
  Rename-Item vits-piper/zh_CN-huayan-medium.onnx model.onnx   # -NewName 只能是叶名，不能带路径
  Remove-Item vits-piper.tar.bz2
}

Write-Host "==> 完成。模型位于 $(Get-Location)"
Write-Host '    现在可：copy ..\config.example.json ..\config.json; python ..\sidecar.py -c ..\config.json'
if ($Tts -eq 'piper') {
  Write-Host ''
  Write-Host '    Piper：把 config.json 的 tts 段改为指向 models/vits-piper 并设 data_dir：'
  Write-Host '      "tts": { "model":"models/vits-piper/model.onnx", "tokens":"models/vits-piper/tokens.txt",'
  Write-Host '               "data_dir":"models/vits-piper/espeak-ng-data", "lexicon":"", "dict_dir":"", "speaker_id":0, "speed":1.0 }'
}
