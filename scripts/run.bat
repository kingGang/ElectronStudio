@echo off
REM Windows 双击入口：转交给 PowerShell 版 run.ps1（参数原样透传）。
REM 优先用 pwsh(7+)，没有则退回 Windows 自带 powershell。
where pwsh >nul 2>nul && (
  pwsh -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" %*
) || (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" %*
)
