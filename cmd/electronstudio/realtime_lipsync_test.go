package main

import "testing"

// sentimentEmotion：按关键词把对话文本判成表情。
func TestSentimentEmotion(t *testing.T) {
	cases := map[string]string{
		"哈哈太好了，我很开心":  "happy",
		"哇，居然是真的":     "surprised",
		"唉，有点难过":      "sad",
		"真讨厌，气死了":     "angry",
		"这个好奇怪，我不懂":   "confused",
		"今天要下雨，记得带伞":  "neutral",
		"":            "neutral",
	}
	for text, want := range cases {
		if got := sentimentEmotion(text); got != want {
			t.Errorf("sentimentEmotion(%q)=%q, 期望 %q", text, got, want)
		}
	}
}

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
