package device

import (
	"testing"
	"time"
)

// TestThrottledLogCollapsesPerFrameSpam 锁住核心行为：逐帧失败只输出首条 + 每 interval 一条，
// 且累计次数不丢。设备掉线时 30fps 下每秒 60 条日志曾把 slog 的全局写锁变成咽喉，
// 连 /api/shutdown 都递不进去（见 throttledLog 注释）。
func TestThrottledLogCollapsesPerFrameSpam(t *testing.T) {
	var tl throttledLog
	const interval = 50 * time.Millisecond

	// 首次必须输出——失败一开始就要看得见。
	ok, n := tl.hit(interval)
	if !ok || n != 1 {
		t.Fatalf("首次失败应输出且累计=1，得到 ok=%v n=%d", ok, n)
	}

	// 紧接着的一连串（模拟逐帧）必须全部被压掉。
	for i := 0; i < 100; i++ {
		if ok, _ := tl.hit(interval); ok {
			t.Fatalf("interval 内第 %d 次不该输出", i)
		}
	}

	// 过了 interval 再输出一条，并带上这段被压掉的累计次数。
	time.Sleep(interval)
	ok, n = tl.hit(interval)
	if !ok {
		t.Fatal("超过 interval 后应输出")
	}
	if n != 101 { // 上面压掉 100 次 + 这次
		t.Fatalf("累计次数应为 101（不能丢），得到 %d", n)
	}
}

// TestThrottledLogResetAllowsImmediateNextReport 恢复后再次失败必须立刻出声，
// 不能被上一段的节流窗口挡住——否则"又掉线了"这条关键信息会迟到 2 秒。
func TestThrottledLogResetAllowsImmediateNextReport(t *testing.T) {
	var tl throttledLog
	const interval = time.Hour // 故意取大：只有 reset 能让它再次输出

	if ok, _ := tl.hit(interval); !ok {
		t.Fatal("首次应输出")
	}
	if ok, _ := tl.hit(interval); ok {
		t.Fatal("窗口内不该输出")
	}

	tl.reset() // 设备恢复正常

	ok, n := tl.hit(interval)
	if !ok || n != 1 {
		t.Fatalf("reset 后再次失败应立刻输出且累计归零，得到 ok=%v n=%d", ok, n)
	}
}
