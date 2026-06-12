# 语音 sidecar（参考实现）

ElectronStudio 的本地语音进程：占用麦克风/扬声器并运行模型推理，通过 WebSocket
与纯 Go 主程序对接。主程序不含任何音频/原生依赖，所有“脏活”集中在此进程。

- ASR / VAD / TTS / 可选唤醒词均由 [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) 提供（ONNX，CPU 即可，树莓派可用）。
- 协议见 [`../../docs/SPEECH.md`](../../docs/SPEECH.md)。

## 快速开始

```bash
# 1) 安装依赖（建议独立虚拟环境）
python -m venv .venv && source .venv/bin/activate   # Windows: .venv\Scripts\activate
pip install -r requirements.txt

# 2) 下载模型（约数百 MB）
bash download_models.sh        # Windows: pwsh ./download_models.ps1

# 3) 配置并运行
cp config.example.json config.json
python sidecar.py -c config.json
# 看到「语音 sidecar 就绪 ws://127.0.0.1:7800」即成功
```

然后让 Go 主程序对接它：

```bash
SPEECH_SIDECAR_URL=ws://127.0.0.1:7800 go run ./cmd/electronstudio
```

此时说话即可被识别并触发对话，助手回复会被合成播放；前端状态灯 ASR/TTS 转为在线。

## 模型

`download_models.{sh,ps1}` 会下载并归一化到与 `config.example.json` 一致的路径：

| 用途 | 模型 | 路径 |
|------|------|------|
| ASR | SenseVoice（中/英/日/韩/粤） | `models/sense-voice/` |
| VAD | Silero | `models/silero_vad.onnx` |
| TTS | VITS-zh（fanchen-C） | `models/vits-zh/` |

唤醒词（KWS）默认关闭（`wake.enabled=false`），此时为「VAD 触发的常听」模式：
检测到一段语音即识别。如需自定义唤醒词，下载一个 sherpa-onnx KWS 模型，填入
`wake` 配置并置 `enabled=true`。

## 工作方式

```
麦克风 ──► VAD ──► 语音段 ──► SenseVoice ASR ──► {asr,final}  ─┐
            └─► speaking/level ─────────────► {vad}            │ WebSocket
  (可选)  └─► KWS ─► {wake}                                     ├──────────► Go 主程序
                                                               │
  扬声器 ◄── VITS TTS ◄── {speak} ◄───────────────────────────┘
                          {abort} ─► sd.stop()
```

## 备注

- sherpa-onnx 的 Python API 在不同版本间可能略有出入；本实现按 1.10.x 编写，
  关键调用处已注释。若导入或参数报错，请对照所装版本的官方示例微调。
- 模型文件较大，已在 `.gitignore` 中排除，不入库。
- 打包分发时，可用 PyInstaller 把本目录打成各平台单文件可执行，置于
  `sidecars/<platform>/`，随主程序一起发布。
