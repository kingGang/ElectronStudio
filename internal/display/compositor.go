package display

import "sync"

// Compositor 组合"素材片"与"程序动画脸"：某情绪有离线素材就播素材（更精致），
// 否则回退到程序实时动画脸。两者共享同一当前情绪与说话状态，对上层呈现为单一 Face。
type Compositor struct {
	mu       sync.Mutex
	emotion  string
	clip     *ClipSource     // 可为 nil
	fallback *EmotionSource  // 程序动画脸（始终存在）
}

// NewCompositor 创建组合源。clip 可为 nil（无素材时）。
func NewCompositor(clip *ClipSource, fallback *EmotionSource) *Compositor {
	return &Compositor{emotion: "neutral", clip: clip, fallback: fallback}
}

// SetEmotion 实现 Face：同时设置素材片与程序脸。
func (c *Compositor) SetEmotion(e string) {
	c.mu.Lock()
	c.emotion = e
	c.mu.Unlock()
	if c.clip != nil {
		c.clip.SetEmotion(e)
	}
	c.fallback.SetEmotion(e)
}

// SetSpeaking 实现 Face：口型仅作用于程序脸（素材片为预渲染动画）。
func (c *Compositor) SetSpeaking(b bool) {
	c.fallback.SetSpeaking(b)
}

// Frame 实现 Source：当前情绪有素材则播素材，否则用程序脸。
func (c *Compositor) Frame() []byte {
	c.mu.Lock()
	e := c.emotion
	c.mu.Unlock()
	if c.clip != nil && c.clip.Has(e) {
		return c.clip.Frame()
	}
	return c.fallback.Frame()
}
