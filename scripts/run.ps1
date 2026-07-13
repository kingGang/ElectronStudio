<#
ElectronStudio 一键启动（Windows）：
  装依赖 + 下模型 + 构建 + 起语音 sidecar + 跑主程序，缺啥补啥、已就绪则秒起。
  Linux / macOS / 树莓派用 scripts/run.sh。

用法：
  pwsh scripts/run.ps1                         # setup(按需) + go run，监听 :8080
  pwsh scripts/run.ps1 -Build                  # 先编译 bin\electronstudio.exe 再跑
  pwsh scripts/run.ps1 -Addr :8099             # 换监听地址
  pwsh scripts/run.ps1 -Tts piper              # 语音模型 TTS 改用 Piper（默认 vits-zh）
  pwsh scripts/run.ps1 -Mirror https://ghfast.top/   # GitHub 加速（国内下模型快）
  pwsh scripts/run.ps1 -NoSidecar              # 只跑主程序(纯文本链路)，不起语音 sidecar
  pwsh scripts/run.ps1 -SkipModels -SkipDeps   # 已装好时跳过下载/装包
  pwsh scripts/run.ps1 -SetupOnly              # 只准备环境，不启动
#>
[CmdletBinding()]
param(
  [string]$Addr = ':8080',
  [string]$Mirror = '',
  [ValidateSet('vits-zh', 'piper', 'both')][string]$Tts = 'vits-zh',
  [switch]$Build,
  [switch]$NoSidecar,
  [switch]$SkipModels,
  [switch]$SkipDeps,
  [switch]$SetupOnly
)
$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent $PSScriptRoot
$Speech = Join-Path $Root 'sidecars\speech'
Set-Location $Root

function Log($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Die($m) { Write-Host "错误: $m" -ForegroundColor Red; exit 1 }

# 1) 前置检查
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Die "未找到 go，请先安装 Go (>=1.23)：https://go.dev/dl/" }

# 1b) libusb：连真机(ElectronBot)必需的【运行期】依赖。缺了它 electronbot.Probe() 会静默失败、
#     直接回退 Mock——表现就是"机器人明明插着，程序却说没探测到"，极难排查。
#     .gitignore 里有 *.dll，所以它不入库；跟语音模型一样，缺则按需下载。
$LibusbDll = Join-Path $Root 'libusb-1.0.dll'
if (-not (Test-Path $LibusbDll)) {
  Log "下载 libusb（连真机必需；Windows 上 LoadLibrary 会在工作目录找它）"
  $ver = '1.0.28'
  $url = "https://github.com/libusb/libusb/releases/download/v$ver/libusb-$ver.7z"
  if ($Mirror) { $url = $Mirror.TrimEnd('/') + '/' + $url }
  $tmp = Join-Path ([IO.Path]::GetTempPath()) "libusb-$ver"
  New-Item -ItemType Directory -Force $tmp | Out-Null
  $pkg = Join-Path $tmp 'libusb.7z'
  try {
    curl.exe -sSL --fail --max-time 180 -o $pkg $url
    # tar(bsdtar) 是 Win10+ 自带的，能解 7z；不额外依赖 7-Zip。
    Push-Location $tmp; tar.exe -xf $pkg; Pop-Location
    $src = Join-Path $tmp 'VS2022\MS64\dll\libusb-1.0.dll'   # x64 MSVC 版，与 amd64 的 Go 二进制匹配
    if (-not (Test-Path $src)) { throw "解压后未找到 $src" }
    Copy-Item $src $LibusbDll -Force
    Log "libusb 就绪：$LibusbDll"
  }
  catch {
    Write-Host "警告: 下载 libusb 失败（$($_.Exception.Message)）。真机将连不上、自动回退 Mock。" -ForegroundColor Yellow
    Write-Host "      可手动从 https://github.com/libusb/libusb/releases 取 VS2022/MS64/dll/libusb-1.0.dll 放到仓库根目录。" -ForegroundColor Yellow
  }
  finally { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
}

$Py = $null
if (-not $NoSidecar) {
  foreach ($c in 'python', 'py', 'python3') { if (Get-Command $c -ErrorAction SilentlyContinue) { $Py = $c; break } }
  if (-not $Py) { Die "未找到 python（语音 sidecar 需要）。装好后重试，或加 -NoSidecar 只跑文本链路。" }
}

$SidecarUrl = 'ws://127.0.0.1:7800'
$Vpy = $null

# 2) 语音 sidecar：虚拟环境 + 依赖
if (-not $NoSidecar) {
  $Venv = Join-Path $Speech '.venv'
  if (-not (Test-Path $Venv)) { Log "创建 Python 虚拟环境 $Venv"; & $Py -m venv $Venv }
  $Vpy = Join-Path $Venv 'Scripts\python.exe'
  if (-not (Test-Path $Vpy)) { $Vpy = Join-Path $Venv 'bin/python' }   # 兼容 pwsh on *nix

  if (-not $SkipDeps) {
    Log "安装语音 sidecar 依赖 (pip install -r requirements.txt)"
    & $Vpy -m pip install --upgrade pip | Out-Null
    & $Vpy -m pip install -r (Join-Path $Speech 'requirements.txt')
  }

  # 3) 模型：缺则下载
  if (-not $SkipModels -and -not (Test-Path (Join-Path $Speech 'models\sense-voice\model.onnx'))) {
    Log "下载语音模型 (ASR=SenseVoice / VAD=Silero / TTS=$Tts)"
    & (Join-Path $Speech 'download_models.ps1') -Mirror $Mirror -Tts $Tts
  }
  else {
    Log "语音模型已存在，跳过下载"
  }

  # 4) sidecar 配置
  $cfg = Join-Path $Speech 'config.json'
  if (-not (Test-Path $cfg)) {
    Log "生成 sidecar 配置 config.json（复制自 config.example.json）"
    Copy-Item (Join-Path $Speech 'config.example.json') $cfg
  }
}

# 5) 构建（可选）
$Bin = $null
if ($Build) {
  Log "编译主程序 -> bin\electronstudio.exe"
  New-Item -ItemType Directory -Force (Join-Path $Root 'bin') | Out-Null
  $Bin = Join-Path $Root 'bin\electronstudio.exe'
  $env:CGO_ENABLED = '0'
  go build -o $Bin ./cmd/electronstudio
}

if ($SetupOnly) { Log "准备完成（-SetupOnly）。"; exit 0 }

# 6) 启动 sidecar（后台）+ 主程序；退出时清理 sidecar
$side = $null
try {
  if (-not $NoSidecar) {
    Log "启动语音 sidecar ($SidecarUrl)"
    $side = Start-Process -FilePath $Vpy -ArgumentList 'sidecar.py', '-c', 'config.json' `
      -WorkingDirectory $Speech -PassThru -NoNewWindow
    $env:SPEECH_SIDECAR_URL = $SidecarUrl
    $ready = $false
    for ($i = 0; $i -lt 30; $i++) {
      if ($side.HasExited) { Die "sidecar 启动失败，见上方日志" }
      try {
        $t = New-Object Net.Sockets.TcpClient; $t.Connect('127.0.0.1', 7800); $t.Close()
        $ready = $true; break
      }
      catch { Start-Sleep -Milliseconds 500 }
    }
    if ($ready) { Log "sidecar 已就绪" } else { Log "sidecar 未及时就绪，继续启动（主程序会自动重连）" }
  }

  Log "启动主程序 (addr=$Addr) -> http://localhost$Addr"
  if ($Bin) { & $Bin -addr $Addr } else { go run ./cmd/electronstudio -addr $Addr }
}
finally {
  if ($side -and -not $side.HasExited) { Log "关闭语音 sidecar"; $side.Kill() }
}
