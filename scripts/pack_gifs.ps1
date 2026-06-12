# 批量把 GIF 打包成各情绪精灵图集（TexturePacker JSON-Array），供 ElectronStudio 表情使用。
#
# 用法：  pwsh scripts/pack_gifs.ps1 -Src .\gifs -Out .\emotions
#   gifs\happy.gif → emotions\happy.png + emotions\happy.json（情绪名 = GIF 文件名）
#
# 依赖：ffmpeg + ffprobe + TexturePacker（命令行，均需在 PATH）
param(
  [string]$Src = ".\gifs",
  [string]$Out = ".\emotions",
  [int]$Size = 240,
  [int]$MaxSize = 4096
)
$ErrorActionPreference = 'Stop'

foreach ($t in 'ffmpeg', 'ffprobe', 'TexturePacker') {
  if (-not (Get-Command $t -ErrorAction SilentlyContinue)) { throw "缺少依赖：$t" }
}
New-Item -ItemType Directory -Force $Out | Out-Null

$gifs = Get-ChildItem $Src -File | Where-Object { $_.Extension -match '^\.gif$' -or $_.Extension -match '^\.GIF$' }
if (-not $gifs) { throw "在 $Src 没找到 *.gif" }

foreach ($gif in $gifs) {
  $name = $gif.BaseName
  Write-Host "==> $name"
  $tmp = Join-Path $env:TEMP ("es_pack_" + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Force $tmp | Out-Null
  try {
    # 抽帧并缩放到 Size×Size
    & ffmpeg -y -v error -i $gif.FullName -vf "scale=${Size}:${Size}:flags=lanczos" (Join-Path $tmp '%04d.png')
    $nframes = (Get-ChildItem $tmp -Filter *.png).Count

    # 取 GIF 平均帧率 → 整数 fps
    $rate = (& ffprobe -v error -select_streams v:0 -show_entries stream=avg_frame_rate -of csv=p=0 $gif.FullName) | Select-Object -First 1
    $fps = 15
    if ($rate -match '^\s*(\d+)\s*/\s*(\d+)\s*$' -and [int]$Matches[2] -gt 0) { $fps = [int][math]::Round([int]$Matches[1] / [int]$Matches[2]) }
    elseif ($rate -match '^\s*(\d+)\s*$') { $fps = [int]$Matches[1] }
    if ($fps -lt 1) { $fps = 15 }

    # TexturePacker 打包为 JSON (Array)；输入文件夹放最后（位置参数）。
    & TexturePacker `
      --format json-array `
      --sheet (Join-Path $Out "$name.png") `
      --data (Join-Path $Out "$name.json") `
      --trim-mode Trim `
      --disable-rotation `
      --max-size $MaxSize `
      --quiet `
      $tmp

    # 写入 fps（顶层）
    $jsonPath = Join-Path $Out "$name.json"
    $j = Get-Content $jsonPath -Raw | ConvertFrom-Json
    $j | Add-Member -NotePropertyName fps -NotePropertyValue $fps -Force
    $j | ConvertTo-Json -Depth 30 | Set-Content $jsonPath -Encoding UTF8

    Write-Host "    -> $name.png + $name.json（$nframes 帧, fps=$fps）"
  }
  finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
}
Write-Host "完成。把 $Out 放到与 config.json 同目录即可。"
