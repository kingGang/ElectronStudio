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
import base64
import io
import json
import queue
import sys
import threading
import time
from dataclasses import dataclass

import numpy as np
import sounddevice as sd
import websockets

try:
    import sherpa_onnx
except ImportError:
    sys.exit("缺少 sherpa-onnx，请先 pip install -r requirements.txt")

try:
    import soundfile as sf  # 较新版 libsndfile 内置 Ogg/Opus 解码（用于播放小智自带 TTS）
except ImportError:
    sf = None


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

    @property
    def mic_gain(self) -> float:
        """麦克风软件增益：采集音量过低时放大（带限幅防爆音）。默认 1.0=不放大。"""
        return float(self.raw.get("audio", {}).get("mic_gain", 1.0))

    @property
    def noise_gate(self) -> float:
        """噪声门：放大后块 RMS 低于此值则整块清零当静音。用于滤掉某些 USB 麦的恒定噪声底/
        低频嗡声（会被 Silero VAD 误判为持续说话、导致永不分段）。0=关闭。"""
        return float(self.raw.get("audio", {}).get("noise_gate", 0.0))


def load_config(path: str) -> Config:
    with open(path, "r", encoding="utf-8") as f:
        return Config(json.load(f))


def build_keywords_file(keywords, tokens_path, tokens_type, out_path):
    """把配置里的纯文本唤醒词（如 ["小电小电"]）转成 KWS 需要的 token 串并写入文件。
    中文走拼音 token（tokens_type=ppinyin），每行格式：<token 空格分隔> @<原文>。
    返回写入的文件路径；keywords 为空时返回 None。"""
    keywords = [k for k in (keywords or []) if k.strip()]
    if not keywords:
        return None
    rows = sherpa_onnx.text2token(keywords, tokens=tokens_path, tokens_type=tokens_type)
    lines = [" ".join(toks) + " @" + kw for kw, toks in zip(keywords, rows)]
    with open(out_path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    return out_path


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
                    lexicon=t.get("lexicon", ""),   # VITS-zh：拼音词典
                    dict_dir=t.get("dict_dir", ""),  # VITS-zh：jieba 词典目录
                    data_dir=t.get("data_dir", ""),  # Piper：espeak-ng-data 目录
                ),
                num_threads=1,
            ),
        )
        self.tts = sherpa_onnx.OfflineTts(tts_cfg)
        self.tts_sid = int(t.get("speaker_id", 0))
        self.tts_speed = float(t.get("speed", 1.0))

        # --- 可选：唤醒词 KWS ---
        self.spotter = None
        self.wake_enabled = False
        self.wake_window = 8.0        # 唤醒后保持收听的窗口（秒）
        self.wake_keywords = []       # 用于从识别文本里剔除唤醒词本身
        w = cfg.raw.get("wake", {})
        if w.get("enabled"):
            self.wake_keywords = [k for k in (w.get("keywords") or []) if k.strip()]
            # 配置里写了纯文本唤醒词 → 自动转 token 生成 keywords_file；否则用现成文件。
            kw_file = build_keywords_file(
                self.wake_keywords,
                tokens_path=w["tokens"],
                tokens_type=w.get("tokens_type", "ppinyin"),
                out_path=w["keywords_file"],
            ) or w["keywords_file"]
            kwargs = dict(
                tokens=w["tokens"], encoder=w["encoder"], decoder=w["decoder"],
                joiner=w["joiner"], keywords_file=kw_file,
            )
            if w.get("keywords_score") is not None:
                kwargs["keywords_score"] = float(w["keywords_score"])
            if w.get("keywords_threshold") is not None:
                kwargs["keywords_threshold"] = float(w["keywords_threshold"])
            self.spotter = sherpa_onnx.KeywordSpotter(**kwargs)
            self.wake_enabled = True
            self.wake_window = float(w.get("window_seconds", 8))
            print(f"唤醒词已启用: {self.wake_keywords} 收听窗口 {self.wake_window}s", file=sys.stderr)

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
        self._mic_gain = cfg.mic_gain
        self._noise_gate = cfg.noise_gate
        self._stream = None
        self._running = True
        self._last_speaking = False
        self._kws_stream = self.eng.spotter.create_stream() if self.eng.spotter else None
        self._awake_until = 0.0  # 门控模式：>now 时处于收听窗口内才转写

        # 播放队列 + 单线程串行播放：保证「逐句流式」音频按到达顺序、不重叠地播。
        self._play_q: "queue.Queue" = queue.Queue()
        self._play_thread = threading.Thread(target=self._play_worker, daemon=True)
        self._play_thread.start()

    # ---- 麦克风 ----
    def _mic_callback(self, indata, frames, time_info, status):
        # 在 sounddevice 的音频线程中执行：只做最轻量的入队。
        if status:
            print("音频状态:", status, file=sys.stderr)
        block = indata[:, 0].copy()
        block -= np.mean(block)  # 去直流偏置（某些 USB 麦有恒定 DC，会被 VAD 误判为说话）
        if self._mic_gain != 1.0:
            np.clip(block * self._mic_gain, -1.0, 1.0, out=block)  # 放大并限幅，防爆音
        # 注意：噪声门【不】在这里做——它会把唤醒词音节间隙清零、切碎词，KWS 就认不出了。
        # 噪声门只在 VAD 那条路单独做（见 process_loop），KWS 拿到的是连续音频。
        self._audio_q.put(block)

    def start_mic(self):
        self._stream = sd.InputStream(
            samplerate=self.sr, channels=1, dtype="float32",
            blocksize=self.BLOCK, callback=self._mic_callback,
            device=self.cfg.input_device,  # None=系统默认；可指定 ElectronBot 板载 USB 麦
        )
        self._stream.start()

    def stop(self):
        self._running = False
        self._play_q.put(None)  # 解阻塞播放线程
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
                    self._awake_until = time.monotonic() + self.eng.wake_window
                    print(f"[wake] 命中唤醒词: {kw}", file=sys.stderr)
                    await self._emit({"type": "wake", "keyword": kw})

            # 2) VAD：喂入并上报说话状态 + 电平（驱动前端波形）。
            #    噪声门只作用于 VAD：低于门限的块当静音，滤掉恒定噪声底，VAD 才能正确收尾分段；
            #    KWS 不受影响（上面用的是未过门的 block），唤醒词不被切碎。
            vad_block = block
            if self._noise_gate > 0.0 and np.sqrt(np.mean(block ** 2)) < self._noise_gate:
                vad_block = np.zeros_like(block)
            self.eng.vad.accept_waveform(vad_block)
            speaking = self.eng.vad.is_speech_detected()
            level = float(np.sqrt(np.mean(block ** 2)))  # RMS 作为电平近似
            if speaking != self._last_speaking:
                self._last_speaking = speaking
            await self._emit({"type": "vad", "speaking": bool(speaking), "level": min(level * 4, 1.0)})

            # 3) 完整语音段就绪 → （门控）离线识别 → 上报最终结果。
            #    门控模式(wake.enabled)：只有在唤醒窗口内才转写，否则丢弃语音段、不搭理。
            while not self.eng.vad.empty():
                segment = self.eng.vad.front
                samples = np.array(segment.samples, dtype=np.float32)  # 必须在 pop() 之前读，front 是内部引用，pop 后失效→空段
                self.eng.vad.pop()
                if self.eng.wake_enabled and time.monotonic() >= self._awake_until:
                    continue  # 未唤醒：丢弃语音段，不浪费算力转写
                # 识别较重，放执行器，避免阻塞事件循环。
                text = await self.loop.run_in_executor(None, self.eng.transcribe, samples, self.sr)
                _rms = float(np.sqrt(np.mean(samples ** 2))) if samples.size else 0.0
                print(f"[asr-debug] 段长={samples.size/self.sr:.2f}s RMS={_rms:.4f} 转写='{text}'", file=sys.stderr)
                if not text:
                    continue
                if self.eng.wake_enabled:
                    # 剔除识别文本里的唤醒词本身（如「小电小电现在几点」→「现在几点」）；
                    # 整句就是唤醒词时剔空 → 跳过，不触发对话。
                    for kw in self.eng.wake_keywords:
                        text = text.replace(kw, "")
                    text = text.strip(" ，。,.!！?？、")
                    if not text:
                        continue
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
                    self._play_q.put(("speak", text))  # 入队，由播放线程串行处理
            elif msg.get("type") == "play":
                # 主程序送来一段已编码音频（如小智自带 TTS 的 Ogg/Opus），入队串行解码播放。
                data = msg.get("data", "")
                if data:
                    self._play_q.put(("audio", base64.b64decode(data)))
            elif msg.get("type") == "abort":
                self._drain_play()  # 清空待播队列
                sd.stop()           # 立即停止当前播放
        self._running = False

    def _drain_play(self):
        """清空播放队列（打断时用）。"""
        try:
            while True:
                self._play_q.get_nowait()
        except queue.Empty:
            pass

    def _play_worker(self):
        """单线程串行播放：保证逐句音频按到达顺序、不重叠。"""
        while True:
            item = self._play_q.get()
            if item is None or not self._running:
                if not self._running:
                    return
                continue
            kind, payload = item
            try:
                if kind == "speak":
                    samples, sr = self.eng.synthesize(payload)
                elif kind == "audio":
                    if sf is None:
                        print("无法播放音频：缺少 soundfile，请 pip install soundfile", file=sys.stderr)
                        continue
                    samples, sr = sf.read(io.BytesIO(payload), dtype="float32", always_2d=False)
                else:
                    continue
                sd.play(samples, sr, device=self.cfg.output_device)
                sd.wait()
            except Exception as e:  # 播放失败不应拖垮会话/播放线程
                print("播放失败:", kind, e, file=sys.stderr)


# ---------------------------------------------------------------------------
# WebSocket 服务
# ---------------------------------------------------------------------------
async def serve(cfg: Config, engines: Engines):
    loop = asyncio.get_running_loop()

    async def handler(ws):
        print("主程序已连接", ws.remote_address, file=sys.stderr)
        session = Session(ws, engines, cfg, loop)
        try:
            session.start_mic()
        except Exception as e:
            # 没有可用麦克风时不应中断会话：仍可提供 TTS（合成播放），只是收不到语音输入。
            print("麦克风打开失败，仅启用 TTS（无 ASR 输入）：", e, file=sys.stderr)
        try:
            # 并行跑"上行处理"与"下行命令"，任一结束即收尾。
            await asyncio.gather(session.process_loop(), session.handle_incoming())
        finally:
            session.stop()
            print("主程序已断开", file=sys.stderr)

    # max_size 放大到 16MB：播放消息(base64 音频)可能超过默认 1MB 上限。
    async with websockets.serve(handler, cfg.ws_host, cfg.ws_port, max_size=16 * 1024 * 1024):
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
