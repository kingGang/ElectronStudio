package display

import (
	"testing"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// 各情绪目标参数应可区分（颜色/眉向/眼型），保证表情辨识度。
func TestSDFEmotionTargetsDistinct(t *testing.T) {
	if emotionTarget("happy").mouthCurve <= 0 {
		t.Error("happy 应上扬微笑(mouthCurve>0)")
	}
	if a := emotionTarget("angry"); a.col[0] <= a.col[2] {
		t.Error("angry 主色应偏暖(col.R>col.B)")
	}
	if s := emotionTarget("sad"); s.col[2] <= s.col[0] {
		t.Error("sad 主色应偏蓝(col.B>col.R)")
	}
	if emotionTarget("sad").browAngle <= 0 {
		t.Error("sad 眉内端应上挑(browAngle>0)")
	}
	if emotionTarget("angry").browAngle >= 0 {
		t.Error("angry 眉内端应下压(browAngle<0)")
	}
	if emotionTarget("sad").brow == 0 || emotionTarget("angry").brow == 0 {
		t.Error("sad/angry 应显示眉条(brow>0)")
	}
	if emotionTarget("neutral").brow != 0 {
		t.Error("neutral 不应有眉条")
	}
	if emotionTarget("surprised").tall <= 0 {
		t.Error("surprised 眼应更竖长(tall>0)")
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

// 换情绪后应重新推帧（返回非 nil）。
func TestSDFEmotionChangePushesFrame(t *testing.T) {
	s := NewSDFFaceSource()
	for i := 0; i < 20; i++ {
		s.Frame()
	}
	s.SetEmotion("angry")
	got := false
	for i := 0; i < 8; i++ {
		if s.Frame() != nil {
			got = true
			break
		}
	}
	if !got {
		t.Error("换情绪后应推新帧")
	}
}

// 说话时嘴应张开。
func TestSDFSpeakingMouth(t *testing.T) {
	s := NewSDFFaceSource()
	closed := s.mouthOpenEff()
	s.SetSpeaking(true)
	if open := s.mouthOpenEff(); open <= closed {
		t.Errorf("说话应张嘴：closed=%.2f open=%.2f", closed, open)
	}
}

// SDF 基元符号正确性。
func TestSDFPrimitives(t *testing.T) {
	if sdfEllipse(120, 120, 120, 120, 10, 10) >= 0 {
		t.Error("椭圆中心应在内(距离<0)")
	}
	if sdfRoundBox(120, 120, 120, 120, 20, 30, 8) >= 0 {
		t.Error("圆角矩形中心应在内(距离<0)")
	}
	if sdfRoundBox(200, 200, 120, 120, 20, 30, 8) <= 0 {
		t.Error("远点应在外(距离>0)")
	}
}

// SDFFaceSource 应满足 FallbackFace 与 MouthLeveler。
func TestSDFImplementsInterfaces(t *testing.T) {
	var _ FallbackFace = NewSDFFaceSource()
	var _ MouthLeveler = NewSDFFaceSource()
}
