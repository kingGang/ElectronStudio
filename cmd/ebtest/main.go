// ebtest 向 ElectronBot 真机推一张固定的“四色象限”测试图，用于核对屏幕对齐：
// 方向(是否翻转/旋转)、RGB 字节序、以及像素偏移。
//
//	左上=红 右上=绿 左下=蓝 右下=白
//
// 据屏幕实际显示即可判断：
//   - 红块若显示在右上 → 上下/左右翻转或旋转
//   - 红块显示成蓝色   → 字节序是 BGR(需翻转 R/B)
//   - 象限分界线斜着走/错位 → 行跨距(stride)或起始偏移不对
//
// 用法（先停掉占用设备的 electronstudio）：
//
//	CGO_ENABLED=0 go run ./cmd/ebtest
package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/kingGang/ElectronStudio/internal/robot"
	"github.com/kingGang/ElectronStudio/internal/robot/electronbot"
)

const (
	w = 240
	h = 240
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	dev := electronbot.New(log)
	if err := dev.Connect(); err != nil {
		log.Error("连接失败", "err", err)
		os.Exit(1)
	}
	defer dev.Close()

	img := make([]byte, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 3
			var r, g, b byte
			switch {
			case y < h/2 && x < w/2: // 左上 红
				r = 255
			case y < h/2 && x >= w/2: // 右上 绿
				g = 255
			case y >= h/2 && x < w/2: // 左下 蓝
				b = 255
			default: // 右下 白
				r, g, b = 255, 255, 255
			}
			img[i], img[i+1], img[i+2] = r, g, b
		}
	}
	if err := dev.SetImage(img); err != nil {
		log.Error("SetImage 失败", "err", err)
		os.Exit(1)
	}
	dev.SetJointAngles(robot.Joints{}, false)

	log.Info("开始推送四色象限测试图(左上红/右上绿/左下蓝/右下白)，持续 30 秒")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := dev.Sync(); err != nil {
			log.Warn("Sync", "err", err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	log.Info("结束")
}
