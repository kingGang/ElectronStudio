// Package gesture 抽象手势识别能力。
//
// ElectronBot 虽有硬件手势传感器，但其数据不在 USB 自定义协议中（该协议只传图像 +
// 32 字节舵机反馈），无法获取。因此手势识别走【视觉】：由独立的 gesture sidecar
// （MediaPipe 等）读摄像头实时识别手势，经本地 WebSocket 把手势事件推给主程序——
// 架构与 internal/speech 完全对称。无 sidecar 时用 Mock，链路照常可跑通。
package gesture

import "context"

// Event 是一条手势事件。
type Event struct {
	Name       string  // 手势名，如 wave / thumbs_up / open_palm / victory / fist
	Confidence float32 // 置信度 [0,1]，可选
}

// Service 抽象手势识别。
type Service interface {
	// Start 启动服务（如连接 sidecar）。
	Start(ctx context.Context) error
	// Events 返回手势事件流。
	Events() <-chan Event
	// Close 释放资源。
	Close() error
}
