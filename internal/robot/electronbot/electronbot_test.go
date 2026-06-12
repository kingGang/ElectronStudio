package electronbot

import (
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

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
