package display

import "sync"

// Compositor 组合三种屏幕画面源，对上层呈现为单一 Face：
//   - 摄像头（开启时优先，显示实时画面）；
//   - 离线素材片（某情绪有素材就播，更精致）；
//   - 程序实时动画脸（兜底，眨眼/口型）。
type Compositor struct {
	mu         sync.Mutex
	emotion    string
	showCamera bool

	camera   *CameraSource  // 可为 nil
	clip     *ClipSource    // 可为 nil
	fallback *EmotionSource // 始终存在
}

// NewCompositor 创建组合源。camera / clip 可为 nil。
func NewCompositor(camera *CameraSource, clip *ClipSource, fallback *EmotionSource) *Compositor {
	return &Compositor{emotion: "neutral", camera: camera, clip: clip, fallback: fallback}
}

// SetEmotion 实现 Face。
func (c *Compositor) SetEmotion(e string) {
	c.mu.Lock()
	c.emotion = e
	c.mu.Unlock()
	if c.clip != nil {
		c.clip.SetEmotion(e)
	}
	c.fallback.SetEmotion(e)
}

// SetSpeaking 实现 Face（口型仅作用于程序脸）。
func (c *Compositor) SetSpeaking(b bool) {
	c.fallback.SetSpeaking(b)
}

// SetCamera 切换是否显示摄像头画面。关闭时强制程序脸重渲染，以覆盖屏上的摄像头残留帧。
func (c *Compositor) SetCamera(on bool) {
	c.mu.Lock()
	c.showCamera = on
	c.mu.Unlock()
	if !on {
		c.fallback.Invalidate()
	}
}

// Frame 实现 Source：按优先级取帧。摄像头模式下只取摄像头帧（nil=沿用上一帧，避免与脸闪烁）。
func (c *Compositor) Frame() []byte {
	c.mu.Lock()
	e := c.emotion
	cam := c.showCamera
	c.mu.Unlock()

	if cam && c.camera != nil {
		return c.camera.Frame()
	}
	if c.clip != nil && c.clip.Has(e) {
		return c.clip.Frame()
	}
	return c.fallback.Frame()
}
