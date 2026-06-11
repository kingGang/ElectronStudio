package choreography

import (
	"context"
	"testing"
	"time"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// TestSampleAtInterpolation 验证关键帧线性插值：端点取边界值，中点取均值。
func TestSampleAtInterpolation(t *testing.T) {
	frames := []Keyframe{
		{AtMs: 0, Angles: robot.Joints{0, 0, 0, 0, 0, 0}},
		{AtMs: 100, Angles: robot.Joints{10, 0, 0, 0, 0, 0}},
	}

	// 早于首帧 → 首帧。
	if got := SampleAt(frames, -50); got[0] != 0 {
		t.Errorf("起点前应为 0, 实际 %v", got[0])
	}
	// 中点 → 线性插值到一半。
	if got := SampleAt(frames, 50); got[0] != 5 {
		t.Errorf("中点应为 5, 实际 %v", got[0])
	}
	// 晚于末帧 → 末帧。
	if got := SampleAt(frames, 999); got[0] != 10 {
		t.Errorf("末帧后应为 10, 实际 %v", got[0])
	}
}

// TestPlayDrivesTransport 验证：播放动作后传输层被驱动，且最终停在末帧姿态。
func TestPlayDrivesTransport(t *testing.T) {
	mock := robot.NewMock(nil)
	if err := mock.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	// 高帧率 + 短动作，确保测试快速完成。
	eng := NewEngine(mock, WithFPS(200))
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

	// 等待动作播放完成（末帧 30ms + 余量）。
	waitUntil(t, 500*time.Millisecond, func() bool {
		return mock.JointAngles()[0] == 45
	})
	if mock.SyncCount() == 0 {
		t.Error("传输层未被驱动 (SyncCount 为 0)")
	}
}

// TestPlayUnknownAction 验证：播放未注册动作返回错误。
func TestPlayUnknownAction(t *testing.T) {
	eng := NewEngine(robot.NewMock(nil))
	if err := eng.Play(context.Background(), "nope", 1); err == nil {
		t.Fatal("未知动作应返回错误")
	}
}

// TestStopInterrupts 验证：Stop 能打断正在循环播放的动作。
func TestStopInterrupts(t *testing.T) {
	mock := robot.NewMock(nil)
	_ = mock.Connect()
	eng := NewEngine(mock, WithFPS(200))
	eng.Register(Action{
		Name:  "loop",
		Loops: -1, // 无限循环
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

	// 停止后同步次数应不再增长。
	time.Sleep(50 * time.Millisecond)
	n1 := mock.SyncCount()
	time.Sleep(80 * time.Millisecond)
	if n2 := mock.SyncCount(); n2 != n1 {
		t.Errorf("Stop 后仍在驱动: %d -> %d", n1, n2)
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
