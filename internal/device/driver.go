// Package device 提供机器人设备的统一驱动循环。
//
// ElectronBot 的 Sync 是"图像 + 舵机角度一次发完、并读回反馈"的原子操作，因此
// 必须由【单一循环】拥有设备：动作编排只更新目标姿态，画面源只更新画面，
// 由 Driver 以固定帧率把"当前姿态 + 当前画面"一并 Sync 给设备，并把画面/反馈
// 同步广播给 UI（回调）。这避免了多处各自 Sync 造成的图像/姿态错位。
package device

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kingGang/ElectronStudio/internal/display"
	"github.com/kingGang/ElectronStudio/internal/robot"
)

// Driver 以固定帧率驱动机器人传输层。
type Driver struct {
	bot    robot.Transport
	source display.Source
	log    *slog.Logger
	fps    int

	// 回调（由上层注入，用于广播给 UI）：
	onFrame  func(rgb []byte)      // 有新画面帧时（240×240 RGB888）
	onJoints func(j robot.Joints)  // 周期上报舵机真实角度

	mu     sync.Mutex
	pose   robot.Joints
	enable bool
}

// NewDriver 创建设备驱动。source 可为 nil（不推画面）。fps<=0 时取 30。
func NewDriver(bot robot.Transport, source display.Source, log *slog.Logger, fps int,
	onFrame func([]byte), onJoints func(robot.Joints)) *Driver {
	if log == nil {
		log = slog.Default()
	}
	if fps <= 0 {
		fps = 30
	}
	return &Driver{bot: bot, source: source, log: log, fps: fps, onFrame: onFrame, onJoints: onJoints}
}

// SetPose 设置目标舵机角度与使能。实现 choreography.PoseSink，供动作编排/手动/跟随写入。
func (d *Driver) SetPose(angles robot.Joints, enable bool) {
	d.mu.Lock()
	d.pose = angles
	d.enable = enable
	d.mu.Unlock()
}

// Run 启动驱动循环，阻塞至 ctx 取消。
func (d *Driver) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second / time.Duration(d.fps))
	defer ticker.Stop()

	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 1) 有新画面则推给设备并广播给 UI。
			if d.source != nil {
				if frame := d.source.Frame(); frame != nil {
					if err := d.bot.SetImage(frame); err != nil {
						d.log.Debug("设置画面失败", "err", err)
					} else if d.onFrame != nil {
						d.onFrame(frame)
					}
				}
			}
			// 2) 取当前姿态，连同画面一并 Sync。
			d.mu.Lock()
			pose, enable := d.pose, d.enable
			d.mu.Unlock()
			_ = d.bot.SetJointAngles(pose, enable)
			if err := d.bot.Sync(); err != nil {
				d.log.Debug("同步失败", "err", err)
				continue
			}
			// 3) 约 1/3 帧率上报一次真实角度（减小 UI 流量）。
			tick++
			if tick%3 == 0 && d.onJoints != nil {
				d.onJoints(d.bot.JointAngles())
			}
		}
	}
}
