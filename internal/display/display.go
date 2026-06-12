// Package display 提供机器人屏幕的画面源抽象。
//
// 设备屏是"只写"的：主机是唯一帧源，把 240×240 RGB888 帧通过 USB 推给设备显示，
// 同一帧同时广播给 UI 镜像，从而做到设备屏与界面精确同步（见 internal/device.Driver）。
//
// Source 是可插拔的画面来源：当前实现表情渲染（EmotionSource）；
// 摄像头来源（主机 webcam / ffmpeg sidecar / 树莓派 V4L2）可后续实现同一接口接入。
package display

// Source 提供要显示在机器人屏幕上的画面。
//
// Frame 返回一帧 240×240 RGB888（robot.ImageBytesRGB888 字节）。
// 返回 nil 表示"暂无新帧"——驱动据此跳过本次图像推送，沿用设备上的上一帧，
// 避免对静态画面（如未变化的表情）重复占用带宽。
type Source interface {
	Frame() []byte
}
