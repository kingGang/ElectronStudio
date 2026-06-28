package robot

import (
	"fmt"
	"log/slog"
	"sync"
)

// Mock 是 Transport 的内存实现，用于无真机环境下的开发与测试。
// 它记录最近一次下发的画面与角度，并以"完美跟随"的方式把目标角度作为反馈返回。
type Mock struct {
	log *slog.Logger

	mu        sync.Mutex
	connected bool
	enabled   bool
	target    Joints // 最近一次设置的目标角度
	feedback  Joints // 模拟读回的真实角度
	lastImage int    // 最近一帧画面的字节数（仅记录长度，避免大块拷贝）
	syncCount int    // Sync 调用次数，便于测试断言
}

// NewMock 创建一个 Mock 传输。logger 为 nil 时使用 slog 默认 logger。
func NewMock(log *slog.Logger) *Mock {
	if log == nil {
		log = slog.Default()
	}
	return &Mock{log: log}
}

// Connect 实现 Transport。
func (m *Mock) Connect() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = true
	m.log.Info("Mock 机器人已连接")
	return nil
}

// SetImage 实现 Transport：仅校验长度并记录，不做真实渲染。
func (m *Mock) SetImage(rgb888 []byte) error {
	if len(rgb888) != ImageBytesRGB888 {
		return fmt.Errorf("robot: 画面字节数不符, 期望 %d 实际 %d", ImageBytesRGB888, len(rgb888))
	}
	m.mu.Lock()
	m.lastImage = len(rgb888)
	m.mu.Unlock()
	return nil
}

// SetJointAngles 实现 Transport。
func (m *Mock) SetJointAngles(angles Joints, enable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.target = angles
	m.enabled = enable
	return nil
}

// Sync 实现 Transport：把目标角度作为反馈（模拟舵机瞬间到位）。
func (m *Mock) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.connected {
		return fmt.Errorf("robot: 未连接")
	}
	m.feedback = m.target
	m.syncCount++
	return nil
}

// JointAngles 实现 Transport。
func (m *Mock) JointAngles() Joints {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.feedback
}

// Connected 实现 Transport。
func (m *Mock) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Speed 实现 Transport：Mock 无真实 USB 连接，返回空。
func (m *Mock) Speed() string { return "" }

// Close 实现 Transport。
func (m *Mock) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = false
	m.log.Info("Mock 机器人已断开")
	return nil
}

// SyncCount 返回 Sync 被调用的次数（测试辅助）。
func (m *Mock) SyncCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.syncCount
}
