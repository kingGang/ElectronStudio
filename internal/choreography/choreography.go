// Package choreography 实现机器人动作编排：把一段由关键帧描述的动作，
// 按帧率插值成连续的舵机角度，驱动 robot.Transport。
//
// 这是从 ElectronBot / Verdure 的动作能力移植而来的纯逻辑模块——不含任何
// 平台相关或原生依赖，因此可在任意平台编译、并以 Mock 传输充分测试。
package choreography

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// 默认参数。
const (
	defaultFPS = 30 // 动作播放帧率
)

// Keyframe 是动作时间轴上的一个关键帧：在 AtMs 毫秒处，舵机应达到 Angles。
type Keyframe struct {
	AtMs   int          `json:"at_ms"`  // 相对动作起点的时间（毫秒）
	Angles robot.Joints `json:"angles"` // 6 轴目标角度（度）
}

// Action 是一段具名动作，由若干关键帧构成。可序列化以便存盘/录制。
type Action struct {
	Name    string     `json:"name"`              // 动作名（唯一），如 wave / nod
	Emotion string     `json:"emotion,omitempty"` // 关联情绪（可选），用于"情绪→动作"映射
	Loops   int        `json:"loops,omitempty"`   // 默认循环次数；0 视为 1，-1 表示无限循环
	Frames  []Keyframe `json:"frames"`            // 关键帧序列（无需预先排序，引擎会按 AtMs 排序）
}

// Engine 负责播放动作。同一时刻只播放一个动作，新的播放会打断旧的。
type Engine struct {
	transport robot.Transport
	fps       int
	log       *slog.Logger

	mu      sync.Mutex
	actions map[string]Action
	cancel  context.CancelFunc // 当前正在播放动作的取消函数
}

// Option 用于配置 Engine。
type Option func(*Engine)

// WithFPS 设置播放帧率。
func WithFPS(fps int) Option {
	return func(e *Engine) {
		if fps > 0 {
			e.fps = fps
		}
	}
}

// WithLogger 设置日志器。
func WithLogger(log *slog.Logger) Option {
	return func(e *Engine) {
		if log != nil {
			e.log = log
		}
	}
}

// NewEngine 创建一个动作编排引擎，驱动给定的传输层。
func NewEngine(transport robot.Transport, opts ...Option) *Engine {
	e := &Engine{
		transport: transport,
		fps:       defaultFPS,
		log:       slog.Default(),
		actions:   make(map[string]Action),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Register 注册（或覆盖）一个动作。
func (e *Engine) Register(a Action) {
	// 复制并按时间排序关键帧，保证播放时的单调推进。
	frames := make([]Keyframe, len(a.Frames))
	copy(frames, a.Frames)
	sort.Slice(frames, func(i, j int) bool { return frames[i].AtMs < frames[j].AtMs })
	a.Frames = frames

	e.mu.Lock()
	e.actions[a.Name] = a
	e.mu.Unlock()
}

// Unregister 删除一个已注册动作。
func (e *Engine) Unregister(name string) {
	e.mu.Lock()
	delete(e.actions, name)
	e.mu.Unlock()
}

// All 返回当前全部动作的副本（按名排序），用于存盘。
func (e *Engine) All() []Action {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Action, 0, len(e.actions))
	for _, a := range e.actions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names 返回已注册的全部动作名（无序）。
func (e *Engine) Names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.actions))
	for name := range e.actions {
		names = append(names, name)
	}
	return names
}

// Lookup 按名查找动作。
func (e *Engine) Lookup(name string) (Action, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.actions[name]
	return a, ok
}

// Play 异步播放一个动作：先打断当前动作，再在后台 goroutine 中按 loops 循环播放。
// loops 为 0 时取动作自身的默认值；找不到动作时立即返回错误。
func (e *Engine) Play(ctx context.Context, name string, loops int) error {
	a, ok := e.Lookup(name)
	if !ok {
		return fmt.Errorf("choreography: 未知动作 %q", name)
	}
	if loops == 0 {
		loops = a.Loops
	}
	if loops == 0 {
		loops = 1
	}

	e.mu.Lock()
	if e.cancel != nil {
		e.cancel() // 打断上一个动作
	}
	playCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.mu.Unlock()

	go func() {
		defer cancel()
		e.log.Debug("开始播放动作", "name", name, "loops", loops)
		for i := 0; loops < 0 || i < loops; i++ {
			if err := e.playOnce(playCtx, a); err != nil {
				if playCtx.Err() == nil {
					e.log.Warn("动作播放出错", "name", name, "err", err)
				}
				return
			}
		}
		e.log.Debug("动作播放结束", "name", name)
	}()
	return nil
}

// Stop 打断当前正在播放的动作（若有）。
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.mu.Unlock()
}

// playOnce 同步播放动作一遍：按帧率推进，对每一帧插值并驱动传输层。
// 该方法不含 goroutine 与共享状态，便于直接进行确定性测试。
func (e *Engine) playOnce(ctx context.Context, a Action) error {
	if len(a.Frames) == 0 {
		return nil
	}
	total := a.Frames[len(a.Frames)-1].AtMs
	frameDur := time.Second / time.Duration(e.fps)
	frameMs := int(frameDur / time.Millisecond)
	if frameMs <= 0 {
		frameMs = 1
	}

	for t := 0; t <= total; t += frameMs {
		if err := e.drive(SampleAt(a.Frames, t)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(frameDur):
		}
	}
	// 确保精确停在最后一帧，避免因取整漏掉终点姿态。
	return e.drive(a.Frames[len(a.Frames)-1].Angles)
}

// drive 把一组角度下发给机器人并执行一次同步。
func (e *Engine) drive(angles robot.Joints) error {
	if err := e.transport.SetJointAngles(angles, true); err != nil {
		return err
	}
	return e.transport.Sync()
}

// SampleAt 在已按 AtMs 升序排列的关键帧上，对时间点 tMs 做线性插值，返回该时刻的角度。
// 早于首帧返回首帧角度，晚于末帧返回末帧角度。导出以便单独测试。
func SampleAt(frames []Keyframe, tMs int) robot.Joints {
	if len(frames) == 0 {
		return robot.Joints{}
	}
	if tMs <= frames[0].AtMs {
		return frames[0].Angles
	}
	last := frames[len(frames)-1]
	if tMs >= last.AtMs {
		return last.Angles
	}
	// 定位 tMs 落入的相邻两帧 [lo, hi]。
	for i := 1; i < len(frames); i++ {
		hi := frames[i]
		if tMs <= hi.AtMs {
			lo := frames[i-1]
			span := hi.AtMs - lo.AtMs
			var out robot.Joints
			if span == 0 {
				return hi.Angles
			}
			ratio := float32(tMs-lo.AtMs) / float32(span)
			for j := 0; j < robot.JointCount; j++ {
				out[j] = lo.Angles[j] + (hi.Angles[j]-lo.Angles[j])*ratio
			}
			return out
		}
	}
	return last.Angles // 理论上不可达
}
