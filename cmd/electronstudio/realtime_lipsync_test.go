package main

import "testing"

// pcmMouthLevel：静音/空→0，大声→张嘴。
func TestPcmMouthLevel(t *testing.T) {
	if l := pcmMouthLevel(make([]byte, 480)); l != 0 {
		t.Errorf("静音应为 0，实为 %.3f", l)
	}
	if l := pcmMouthLevel(nil); l != 0 {
		t.Errorf("空音频应为 0，实为 %.3f", l)
	}
	loud := make([]byte, 480)
	for i := 0; i+1 < len(loud); i += 2 {
		loud[i], loud[i+1] = 0xFF, 0x3F // int16≈16383 (半幅)
	}
	if l := pcmMouthLevel(loud); l <= 0.5 {
		t.Errorf("大声应张嘴(>0.5)，实为 %.3f", l)
	}
}
