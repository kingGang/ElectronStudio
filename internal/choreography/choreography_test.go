package choreography

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// fakeSink 记录最近一次写入的姿态与写入次数，供测试断言。
type fakeSink struct {
	mu    sync.Mutex
	pose  robot.Joints
	count int
}

func (f *fakeSink) SetPose(angles robot.Joints, _ bool) {
	f.mu.Lock()
	f.pose = angles
	f.count++
	f.mu.Unlock()
}
func (f *fakeSink) get() robot.Joints {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pose
}
func (f *fakeSink) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// TestSampleAtInterpolation 验证关键帧线性插值：端点取边界值，中点取均值。
func TestSampleAtInterpolation(t *testing.T) {
	frames := []Keyframe{
		{AtMs: 0, Angles: robot.Joints{0, 0, 0, 0, 0, 0}},
		{AtMs: 100, Angles: robot.Joints{10, 0, 0, 0, 0, 0}},
	}
	if got := SampleAt(frames, -50); got[0] != 0 {
		t.Errorf("起点前应为 0, 实际 %v", got[0])
	}
	if got := SampleAt(frames, 50); got[0] != 5 {
		t.Errorf("中点应为 5, 实际 %v", got[0])
	}
	if got := SampleAt(frames, 999); got[0] != 10 {
		t.Errorf("末帧后应为 10, 实际 %v", got[0])
	}
}

// TestPlayDrivesSink 验证：播放动作后姿态被写入，且最终停在末帧。
func TestPlayDrivesSink(t *testing.T) {
	sink := &fakeSink{}
	eng := NewEngine(sink, WithFPS(200)) // 高帧率 + 短动作，快速完成
	eng.Register(Action{
		Name: "test",
		Frames: []Keyframe{
			{AtMs: 0, Angles: robot.Joints{}},
			{AtMs: 30, Angles: robot.Joints{45, 0, 0, 0, 0, 0}},
		},
	})
	if err := eng.Play(context.Background(), "test", 1); err != nil {
		t.Fatalf("播放失败: %v", err)
	}
	waitUntil(t, 500*time.Millisecond, func() bool { return sink.get()[0] == 45 })
	if sink.n() == 0 {
		t.Error("姿态接收器未被写入")
	}
}

// TestPlayUnknownAction 验证：播放未注册动作返回错误。
func TestPlayUnknownAction(t *testing.T) {
	eng := NewEngine(&fakeSink{})
	if err := eng.Play(context.Background(), "nope", 1); err == nil {
		t.Fatal("未知动作应返回错误")
	}
}

// TestStopInterrupts 验证：Stop 能打断正在循环播放的动作。
func TestStopInterrupts(t *testing.T) {
	sink := &fakeSink{}
	eng := NewEngine(sink, WithFPS(200))
	eng.Register(Action{
		Name:  "loop",
		Loops: -1,
		Frames: []Keyframe{
			{AtMs: 0, Angles: robot.Joints{}},
			{AtMs: 20, Angles: robot.Joints{10, 0, 0, 0, 0, 0}},
		},
	})
	if err := eng.Play(context.Background(), "loop", 0); err != nil {
		t.Fatalf("播放失败: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	eng.Stop()
	time.Sleep(50 * time.Millisecond)
	n1 := sink.n()
	time.Sleep(80 * time.Millisecond)
	if n2 := sink.n(); n2 != n1 {
		t.Errorf("Stop 后仍在写入: %d -> %d", n1, n2)
	}
}

// waitUntil 在超时时间内轮询条件，未满足则失败。
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
