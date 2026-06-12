#!/usr/bin/env python3
"""手势 sidecar 参考实现（MediaPipe）。

读摄像头，用 MediaPipe GestureRecognizer 实时识别手势，通过 WebSocket 把手势事件
推给纯 Go 主程序（主程序作为客户端连接本服务）。与语音 sidecar 架构对称。

协议（JSON 文本帧）：sidecar → 主程序
    {"type":"gesture","name":"thumbs_up","confidence":0.86}

主程序对手势的默认映射（见 cmd）：
    wave→看一眼打招呼  thumbs_up→开心+点头  victory→庆祝  open_palm→停止  fist→摇头

MediaPipe 内置手势类别：Closed_Fist / Open_Palm / Pointing_Up / Thumb_Down /
Thumb_Up / Victory / ILoveYou，这里映射到上面的语义名。

准备：下载手势模型 gesture_recognizer.task
    https://storage.googleapis.com/mediapipe-models/gesture_recognizer/gesture_recognizer/float16/1/gesture_recognizer.task

注意：摄像头是独占设备——若同时启用了主程序的"摄像头上屏"(ffmpeg)，二者会争抢同一摄像头。
"""

import argparse
import asyncio
import json
import sys
import time

import cv2
import numpy as np
import websockets

try:
    import mediapipe as mp
    from mediapipe.tasks import python as mp_python
    from mediapipe.tasks.python import vision as mp_vision
except ImportError:
    sys.exit("缺少 mediapipe，请先 pip install -r requirements.txt")

# MediaPipe 类别名 → 本项目语义名。
GESTURE_MAP = {
    "Open_Palm": "open_palm",
    "Closed_Fist": "fist",
    "Thumb_Up": "thumbs_up",
    "Victory": "victory",
    "Pointing_Up": "point",
    "ILoveYou": "love",
}


def build_recognizer(model_path: str):
    base = mp_python.BaseOptions(model_asset_path=model_path)
    opts = mp_vision.GestureRecognizerOptions(
        base_options=base,
        running_mode=mp_vision.RunningMode.VIDEO,
    )
    return mp_vision.GestureRecognizer.create_from_options(opts)


async def serve(args):
    recognizer = build_recognizer(args.model)

    async def handler(ws):
        print("主程序已连接", file=sys.stderr)
        cap = cv2.VideoCapture(args.camera)
        if not cap.isOpened():
            print("无法打开摄像头", args.camera, file=sys.stderr)
            return
        loop = asyncio.get_running_loop()
        last_name, last_emit = None, 0.0
        try:
            while True:
                ok, frame = await loop.run_in_executor(None, cap.read)
                if not ok:
                    await asyncio.sleep(0.01)
                    continue
                rgb = cv2.cvtColor(frame, cv2.COLOR_BGR2RGB)
                mp_image = mp.Image(image_format=mp.ImageFormat.SRGB, data=rgb)
                ts = int(time.time() * 1000)
                result = recognizer.recognize_for_video(mp_image, ts)

                name, score = None, 0.0
                if result.gestures:
                    top = result.gestures[0][0]
                    if top.category_name and top.category_name != "None":
                        name = GESTURE_MAP.get(top.category_name, top.category_name.lower())
                        score = float(top.score)

                now = time.time()
                # 去抖：仅在手势变化或冷却(1s)后、且置信度足够时上报。
                if name and score >= args.threshold and (name != last_name or now - last_emit > 1.0):
                    last_name, last_emit = name, now
                    msg = json.dumps({"type": "gesture", "name": name, "confidence": round(score, 2)})
                    await ws.send(msg)
                    print("手势:", name, round(score, 2), file=sys.stderr)
                elif not name:
                    last_name = None

                await asyncio.sleep(0.03)  # ~30fps 上限
        except websockets.ConnectionClosed:
            pass
        finally:
            cap.release()
            print("主程序已断开", file=sys.stderr)

    async with websockets.serve(handler, args.ws_host, args.ws_port):
        print(f"手势 sidecar 就绪 ws://{args.ws_host}:{args.ws_port}", file=sys.stderr)
        await asyncio.Future()


def main():
    ap = argparse.ArgumentParser(description="ElectronStudio 手势 sidecar")
    ap.add_argument("--model", default="gesture_recognizer.task", help="MediaPipe 手势模型路径")
    ap.add_argument("--camera", type=int, default=0, help="摄像头索引")
    ap.add_argument("--ws-host", default="127.0.0.1")
    ap.add_argument("--ws-port", type=int, default=7810)
    ap.add_argument("--threshold", type=float, default=0.6, help="置信度阈值")
    args = ap.parse_args()
    try:
        asyncio.run(serve(args))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
