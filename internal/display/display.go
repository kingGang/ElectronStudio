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

// Face 是"会表达情绪的画面源"：在 Source 之上，可被上层设置情绪与说话状态，
// 由实现自己决定如何实时渲染（程序动画脸 EmotionSource、素材片 Compositor 等）。
type Face interface {
	Source
	SetEmotion(emotion string) // AI / 用户设定当前情绪
	SetSpeaking(speaking bool)  // TTS 播放期间为 true，用于口型动画
}

// FallbackFace 是兜底表情脸：在 Face 之上加 Invalidate（从摄像头切回时强制重画一帧，
// 覆盖屏上残留）。程序动画脸 EmotionSource 与 SDF 脸 SDFFaceSource 都实现它，
// 可由 config.face 选择其一作为 Compositor 的兜底。
type FallbackFace interface {
	Face
	Invalidate()
}

// MouthLeveler 是可选能力：按真实说话音量(0..1)驱动口型，让嘴与对话同步。
// SDFFaceSource 实现它；上层(如实时语音下行 RMS)可据此高频喂入。EmotionSource 不实现（节奏兜底）。
type MouthLeveler interface {
	SetMouthLevel(level float64)
}
