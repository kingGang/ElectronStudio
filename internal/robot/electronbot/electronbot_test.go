package electronbot

import (
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// TestShouldClearHalt 锁死"超时不 clear_halt"这条铁律。
//
// 背景（真机代价惨重的一课）：bulk 超时时端点【没有 stall】，此时 clear_halt 会复位两端的数据
// 翻转位，把固件正在跑的 4 段 lockstep 搞失步 → 它永远等不到 224 尾包 → 无超时自旋 → 主控硬死、
// 只能断电复位。官方 .NET SDK 超时后只原地重发、从不 clear_halt。唯一例外是 macOS：IOKit 上超时
// 的传输取消不干净会堵死管道，那是平台缺陷。
//
// 任何人想"顺手"把超时也加回 clear_halt 之前，先看这个测试和 shouldClearHalt 的注释。
func TestShouldClearHalt(t *testing.T) {
	cases := []struct {
		name string
		ret  int32
		goos string
		want bool
	}{
		{"超时/windows：绝不清(清了必失步→硬死)", errTimeout, "windows", false},
		{"超时/linux：绝不清", errTimeout, "linux", false},
		{"超时/darwin：例外，IOKit 会堵死管道", errTimeout, "darwin", true},
		{"真 stall(PIPE)/windows：该清", errPipe, "windows", true},
		{"真 stall(PIPE)/darwin：该清", errPipe, "darwin", true},
		{"掉线：不清", errNoDevice, "windows", false},
		{"成功：不清", 0, "windows", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldClearHalt(c.ret, c.goos); got != c.want {
				t.Fatalf("ret=%d goos=%s: 期望 %v 实际 %v", c.ret, c.goos, c.want, got)
			}
		})
	}
}

// TestSegmentMath 验证分段常量自洽，与整帧字节数一致。
func TestSegmentMath(t *testing.T) {
	if segImageBytes != 43008 {
		t.Fatalf("段图像主体应为 43008, 实际 %d", segImageBytes)
	}
	if segStride != 43200 {
		t.Fatalf("段步长应为 43200, 实际 %d", segStride)
	}
	if tailPacketBytes != 224 {
		t.Fatalf("尾包应为 224, 实际 %d", tailPacketBytes)
	}
	if imageBytes != segments*segStride {
		t.Fatalf("整帧应为段步长之和, imageBytes=%d", imageBytes)
	}
	if imageBytes != robot.ImageBytesRGB888 {
		t.Fatalf("整帧字节数应与屏幕一致, imageBytes=%d", imageBytes)
	}
}

// TestExtraDataRoundTrip 验证角度打包与反馈解析互为逆运算。
func TestExtraDataRoundTrip(t *testing.T) {
	angles := robot.Joints{12.5, -5, 47, 20, 0, 180}
	b := buildExtraData(angles, true)

	if b[0] != 1 {
		t.Fatalf("使能位应为 1, 实际 %d", b[0])
	}
	got := parseFeedback(b[:])
	if got != angles {
		t.Fatalf("角度往返不一致: 期望 %v 实际 %v", angles, got)
	}

	// 使能=false 时首字节为 0。
	if buildExtraData(angles, false)[0] != 0 {
		t.Fatal("使能=false 时首字节应为 0")
	}
}

// TestParseFeedbackShort 验证过短缓冲不致崩溃。
func TestParseFeedbackShort(t *testing.T) {
	if got := parseFeedback([]byte{1, 2, 3}); got != (robot.Joints{}) {
		t.Fatalf("过短缓冲应返回零值, 实际 %v", got)
	}
}
