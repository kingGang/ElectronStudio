#!/usr/bin/env python3
"""语音 sidecar 参考实现。

它是 ElectronStudio 的本地语音进程：占用麦克风/扬声器并运行模型推理，
通过 WebSocket 与纯 Go 主程序对接（主程序不含任何音频/原生依赖）。

协议（见 docs/SPEECH.md）：
    sidecar → 主程序：{"type":"wake","keyword":...}
                      {"type":"vad","speaking":bool,"level":float}
                      {"type":"asr","text":...,"final":bool}
    主程序 → sidecar：{"type":"speak","text":...}
                      {"type":"abort"}

模型由 sherpa-onnx 提供（ASR=SenseVoice，VAD=Silero，TTS=VITS，可选 KWS 唤醒）。
请先用 download_models.{sh,ps1} 下载模型，并按 config.example.json 配置路径。

注意：sherpa-onnx 的 API 在不同版本间可能略有差异，本文件按 1.10.x 编写；
若导入或参数报错，请对照所装版本的官方示例微调（已在关键处标注）。
"""

import argparse
import asyncio
import json
import queue
import sys
import threading
from dataclasses import dataclass

import numpy as np
import sounddevice as sd
import websockets

try:
    import sherpa_onnx
except ImportError:
    sys.exit("缺少 sherpa-onnx，请先 pip install -r requirements.txt")


# ---------------------------------------------------------------------------
# 配置
# ---------------------------------------------------------------------------
@dataclass
class Config:
    raw: dict

    @property
    def ws_host(self) -> str:
        return self.raw.get("ws_host", "127.0.0.1")

    @property
    def ws_port(self) -> int:
        return int(self.raw.get("ws_port", 7800))

    @property
    def sample_rate(self) -> int:
        return int(self.raw.get("sample_rate", 16000))

    @property
    def input_device(self):
        """麦克风设备（名称子串或索引）；空字符串返回 None（系统默认）。"""
        d = self.raw.get("audio", {}).get("input_device", "")
        return d if d not in ("", None) else None

    @property
    def output_device(self):
        """扬声器设备（名称子串或索引）；空字符串返回 None（系统默认）。"""
        d = self.raw.get("audio", {}).get("output_device", "")
        return d if d not in ("", None) else None


def load_config(path: str) -> Config:
    with open(path, "r", encoding="utf-8") as f:
        return Config(json.load(f))


# ---------------------------------------------------------------------------
# 模型引擎：集中加载 ASR / VAD / TTS / 可选 KWS
# ---------------------------------------------------------------------------
class Engines:
    """惰性加载并持有各语音模型。"""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        sr = cfg.sample_rate

        # --- 离线 ASR：SenseVoice ---
        a = cfg.raw["asr"]
        self.recognizer = sherpa_onnx.OfflineRecognizer.from_sense_voice(
            model=a["model"],
            tokens=a["tokens"],
            use_itn=bool(a.get("use_itn", True)),
        )

        # --- VAD：Silero ---
        v = cfg.raw["vad"]
        vad_cfg = sherpa_onnx.VadModelConfig()
        vad_cfg.silero_vad.model = v["model"]
        vad_cfg.silero_vad.threshold = float(v.get("threshold", 0.5))
        vad_cfg.silero_vad.min_silence_duration = float(v.get("min_silence_duration", 0.5))
        vad_cfg.silero_vad.min_speech_duration = float(v.get("min_speech_duration", 0.25))
        vad_cfg.sample_rate = sr
        # buffer_size 给足，避免长句被丢弃。
        self.vad = sherpa_onnx.VoiceActivityDetector(vad_cfg, buffer_size_in_seconds=30)

        # --- TTS：VITS / Piper ---
        t = cfg.raw["tts"]
        tts_cfg = sherpa_onnx.OfflineTtsConfig(
            model=sherpa_onnx.OfflineTtsModelConfig(
                vits=sherpa_onnx.OfflineTtsVitsModelConfig(
                    model=t["model"],
                    tokens=t["tokens"],
                    lexicon=t.get("lexicon", ""),
                    dict_dir=t.get("dict_dir", ""),
                ),
                num_threads=1,
            ),
        )
        self.tts = sherpa_onnx.OfflineTts(tts_cfg)
        self.tts_sid = int(t.get("speaker_id", 0))
        self.tts_speed = float(t.get("speed", 1.0))

        # --- 可选：唤醒词 KWS ---
        self.spotter = None
        w = cfg.raw.get("wake", {})
        if w.get("enabled"):
            self.spotter = sherpa_onnx.KeywordSpotter(
                tokens=w["tokens"],
                encoder=w["encoder"],
                decoder=w["decoder"],
                joiner=w["joiner"],
                keywords_file=w["keywords_file"],
            )

    def transcribe(self, samples: np.ndarray, sr: int) -> str:
        """对一段 PCM（float32, [-1,1]）做离线识别。"""
        stream = self.recognizer.create_stream()
        stream.accept_waveform(sr, samples)
        self.recognizer.decode_stream(stream)
        return stream.result.text.strip()

    def synthesize(self, text: str):
        """合成语音，返回 (np.float32 samples, sample_rate)。"""
        audio = self.tts.generate(text, sid=self.tts_sid, speed=self.tts_speed)
        return np.array(audio.samples, dtype=np.float32), audio.sample_rate


# ---------------------------------------------------------------------------
# 会话：一个已连接的主程序对应一次会话，串起麦克风 → 模型 → WS
# ---------------------------------------------------------------------------
class Session:
    BLOCK = 512  # 每个音频回调的帧数（约 32ms @16k）

    def __init__(self, ws, engines: Engines, cfg: Config, loop):
        self.ws = ws
        self.eng = engines
        self.cfg = cfg
        self.loop = loop
        self.sr = cfg.sample_rate

        self._audio_q: "queue.Queue[np.ndarray]" = queue.Queue()
        self._stream = None
        self._running = True
        self._last_speaking = False
        self._kws_stream = self.eng.spotter.create_stream() if self.eng.spotter else None

    # ---- 麦克风 ----
    def _mic_callback(self, indata, frames, time_info, status):
        # 在 sounddevice 的音频线程中执行：只做最轻量的入队。
        if status:
            print("音频状态:", status, file=sys.stderr)
        self._audio_q.put(indata[:, 0].copy())

    def start_mic(self):
        self._stream = sd.InputStream(
            samplerate=self.sr, channels=1, dtype="float32",
            blocksize=self.BLOCK, callback=self._mic_callback,
            device=self.cfg.input_device,  # None=系统默认；可指定 ElectronBot 板载 USB 麦
        )
        self._stream.start()

    def stop(self):
        self._running = False
        if self._stream:
            self._stream.stop()
            self._stream.close()
            self._stream = None

    # ---- 向主程序发事件（在事件循环线程中调用）----
    async def _emit(self, msg: dict):
        try:
            await self.ws.send(json.dumps(msg, ensure_ascii=False))
        except websockets.ConnectionClosed:
            self._running = False

    # ---- 处理循环：从队列取音频，跑 VAD / KWS / ASR ----
    async def process_loop(self):
        while self._running:
            # 阻塞地取一块音频放到执行器，避免卡住事件循环。
            try:
                block = await self.loop.run_in_executor(None, self._audio_q.get, True, 0.2)
            except queue.Empty:
                continue
            except Exception:
                continue

            # 1) 唤醒词（可选）。
            if self._kws_stream is not None:
                self._kws_stream.accept_waveform(self.sr, block)
                while self.eng.spotter.is_ready(self._kws_stream):
                    self.eng.spotter.decode_stream(self._kws_stream)
                kw = self.eng.spotter.get_result(self._kws_stream)
                if kw:
                    self.eng.spotter.reset_stream(self._kws_stream)
                    await self._emit({"type": "wake", "keyword": kw})

            # 2) VAD：喂入并上报说话状态 + 电平（驱动前端波形）。
            self.eng.vad.accept_waveform(block)
            speaking = self.eng.vad.is_speech_detected()
            level = float(np.sqrt(np.mean(block ** 2)))  # RMS 作为电平近似
            if speaking != self._last_speaking:
                self._last_speaking = speaking
            await self._emit({"type": "vad", "speaking": bool(speaking), "level": min(level * 4, 1.0)})

            # 3) 完整语音段就绪 → 离线识别 → 上报最终结果。
            while not self.eng.vad.empty():
                segment = self.eng.vad.front
                self.eng.vad.pop()
                samples = np.array(segment.samples, dtype=np.float32)
                # 识别较重，放执行器，避免阻塞事件循环。
                text = await self.loop.run_in_executor(None, self.eng.transcribe, samples, self.sr)
                if text:
                    await self._emit({"type": "asr", "text": text, "final": True})

    # ---- 处理来自主程序的下行命令（speak / abort）----
    async def handle_incoming(self):
        async for raw in self.ws:
            try:
                msg = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if msg.get("type") == "speak":
                text = msg.get("text", "")
                if text:
                    await self.loop.run_in_executor(None, self._speak, text)
            elif msg.get("type") == "abort":
                sd.stop()  # 立即停止当前播放
        self._running = False

    def _speak(self, text: str):
        """合成并播放（阻塞，在执行器线程中运行）。"""
        try:
            samples, sr = self.eng.synthesize(text)
            sd.play(samples, sr, device=self.cfg.output_device)
            sd.wait()
        except Exception as e:  # 播放失败不应拖垮会话
            print("TTS 播放失败:", e, file=sys.stderr)


# ---------------------------------------------------------------------------
# WebSocket 服务
# ---------------------------------------------------------------------------
async def serve(cfg: Config, engines: Engines):
    loop = asyncio.get_running_loop()

    async def handler(ws):
        print("主程序已连接", ws.remote_address, file=sys.stderr)
        session = Session(ws, engines, cfg, loop)
        session.start_mic()
        try:
            # 并行跑"上行处理"与"下行命令"，任一结束即收尾。
            await asyncio.gather(session.process_loop(), session.handle_incoming())
        finally:
            session.stop()
            print("主程序已断开", file=sys.stderr)

    async with websockets.serve(handler, cfg.ws_host, cfg.ws_port):
        print(f"语音 sidecar 就绪 ws://{cfg.ws_host}:{cfg.ws_port}", file=sys.stderr)
        await asyncio.Future()  # 永久运行直至进程被终止


def main():
    ap = argparse.ArgumentParser(description="ElectronStudio 语音 sidecar")
    ap.add_argument("-c", "--config", default="config.json", help="配置文件路径")
    ap.add_argument("--list-devices", action="store_true",
                    help="列出所有音频设备(找 ElectronBot 板载 USB 麦克风的名字/索引)后退出")
    args = ap.parse_args()

    if args.list_devices:
        print(sd.query_devices())
        return

    cfg = load_config(args.config)
    print("加载模型中…", file=sys.stderr)
    engines = Engines(cfg)
    print("模型加载完成", file=sys.stderr)

    try:
        asyncio.run(serve(cfg, engines))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
