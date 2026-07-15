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

	still      []byte // 临时静态图（最高优先级，如 MiniMax 生成图）；nil 表示无
	stillTicks int    // 静态图剩余显示帧数
	clipsOff   bool   // true 时跳过离线素材片，全部情绪交给兜底脸（SDF 模式：实时表情接管一切）

	camera   *CameraSource // 可为 nil
	clip     *ClipSource   // 可为 nil
	fallback FallbackFace  // 始终存在（EmotionSource 或 SDFFaceSource）
}

// ShowImage 在设备屏上临时显示一张静态图（240×240 RGB888）持续 seconds 秒，
// 期间盖过摄像头/素材/程序脸；到期自动恢复。用于 MiniMax 文生图等结果上屏。
func (c *Compositor) ShowImage(rgb []byte, seconds int) {
	if len(rgb) == 0 {
		return
	}
	if seconds <= 0 {
		seconds = 8
	}
	buf := make([]byte, len(rgb))
	copy(buf, rgb)
	c.mu.Lock()
	c.still = buf
	c.stillTicks = seconds * driverFrameRate
	c.mu.Unlock()
}

// NewCompositor 创建组合源。camera / clip 可为 nil；fallback 为兜底脸（程序脸或 SDF 脸）。
func NewCompositor(camera *CameraSource, clip *ClipSource, fallback FallbackFace) *Compositor {
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

// SetMouthLevel 按真实说话音量(0..1)驱动口型，让嘴与对话同步。兜底脸支持时(SDF 脸)才生效。
func (c *Compositor) SetMouthLevel(level float64) {
	if ml, ok := c.fallback.(MouthLeveler); ok {
		ml.SetMouthLevel(level)
	}
}

// SetClipsEnabled 开关离线素材片层。关闭时所有情绪都由兜底脸（如 SDF 实时表情）渲染，
// 不再被 emotions/ 里的默认/上传素材盖住。SDF 模式下由 main 关掉，让实时表情真正显现。
func (c *Compositor) SetClipsEnabled(on bool) {
	c.mu.Lock()
	c.clipsOff = !on
	c.mu.Unlock()
	c.fallback.Invalidate() // 切换即强制兜底脸重画一帧，覆盖屏上残留
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
	clipsOff := c.clipsOff
	// 静态图最高优先级：在 TTL 内每帧返回它；到期清除并让程序脸重渲染覆盖。
	if c.still != nil && c.stillTicks > 0 {
		c.stillTicks--
		out := make([]byte, len(c.still))
		copy(out, c.still)
		if c.stillTicks == 0 {
			c.still = nil
			c.fallback.Invalidate()
		}
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()

	if cam && c.camera != nil {
		return c.camera.Frame()
	}
	// 单次调用原子地判定“该情绪有无素材”并取帧，避免 Has()+Frame() 之间的竞态。
	// clipsOff（SDF 模式）时跳过素材层，直接用兜底脸渲染所有情绪。
	if !clipsOff && c.clip != nil {
		if f := c.clip.FrameFor(e); f != nil {
			return f
		}
	}
	return c.fallback.Frame()
}
