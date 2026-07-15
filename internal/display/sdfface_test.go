package display

import (
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// 各情绪目标参数应可区分（颜色/眼睑/眼型），保证表情辨识度。
func TestSDFEmotionTargetsDistinct(t *testing.T) {
	if emotionTarget("happy").squint <= 0 {
		t.Error("happy 应眯眼(squint>0)")
	}
	if a := emotionTarget("angry"); a.colA[0] <= a.colA[2] {
		t.Error("angry 顶色应偏红(colA.R>colA.B)")
	}
	if s := emotionTarget("sad"); s.colB[2] <= s.colB[0] {
		t.Error("sad 底色应偏蓝(colB.B>colB.R)")
	}
	if n := emotionTarget("neutral"); n.colA == n.colB {
		t.Error("眼睛应为上下双色渐变(colA!=colB)")
	}
	if emotionTarget("sad").lidAngle >= 0 {
		t.Error("sad 上眼睑应外端低(lidAngle<0)")
	}
	if emotionTarget("angry").lidAngle <= 0 {
		t.Error("angry 上眼睑应内端低(lidAngle>0)")
	}
	if emotionTarget("surprised").eyeScale <= 1 {
		t.Error("surprised 眼应更大(eyeScale>1)")
	}
	if emotionTarget("confused").tilt == 0 {
		t.Error("confused 应头倾(tilt!=0)")
	}
}

// 首帧应渲染整屏 RGB888。
func TestSDFFrameSize(t *testing.T) {
	s := NewSDFFaceSource()
	if f := s.Frame(); len(f) != robot.ImageBytesRGB888 {
		t.Fatalf("首帧应为整屏 RGB888(%d)，实为 %d", robot.ImageBytesRGB888, len(f))
	}
}

// 静止后应返回 nil（省带宽、护 USB）；换情绪后应重新推帧。
func TestSDFChangeDetection(t *testing.T) {
	s := NewSDFFaceSource()
	s.nextBlink = 1 << 30 // 关眨眼，避免周期性推帧干扰
	nilSeen := false
	for i := 0; i < 80; i++ {
		if s.Frame() == nil {
			nilSeen = true
			break
		}
	}
	if !nilSeen {
		t.Error("表情 settle 后应出现 nil 帧（静止不推帧）")
	}
	s.SetEmotion("happy")
	got := false
	for i := 0; i < 8; i++ {
		if s.Frame() != nil {
			got = true
			break
		}
	}
	if !got {
		t.Error("换情绪后应重新推帧")
	}
}

// 说话且喂入音量时嘴应张开。
func TestSDFSpeakingMouth(t *testing.T) {
	s := NewSDFFaceSource()
	closed := s.mouthOpenEff()
	s.SetSpeaking(true)
	s.SetMouthLevel(0.9)
	if open := s.mouthOpenEff(); open <= closed {
		t.Errorf("说话+音量应张嘴：closed=%.2f open=%.2f", closed, open)
	}
}

// SDF 基元符号正确性。
func TestSDFPrimitives(t *testing.T) {
	if sdfEllipse(120, 120, 120, 120, 10, 10) >= 0 {
		t.Error("椭圆中心应在内(距离<0)")
	}
	if sdfEllipse(200, 200, 120, 120, 10, 10) <= 0 {
		t.Error("远点应在外(距离>0)")
	}
	if d := sdfSeg(0, 5, 0, 0, 10, 0); d < 4.9 || d > 5.1 {
		t.Errorf("点到线段距离应≈5，实为 %.2f", d)
	}
}

// SDFFaceSource 应满足 FallbackFace 与 MouthLeveler。
func TestSDFImplementsInterfaces(t *testing.T) {
	var _ FallbackFace = NewSDFFaceSource()
	var _ MouthLeveler = NewSDFFaceSource()
}
