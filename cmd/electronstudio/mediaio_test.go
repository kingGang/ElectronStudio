package main

import "testing"

func TestStripThink(t *testing.T) {
	cases := map[string]string{
		"<think>推理过程</think>你好":                "你好",
		"你好":                                  "你好",
		"<think>a</think>\n\n答案在这里":           "答案在这里",
		"前<think>x</think>后":                  "前后",
		"<think>多行\n推理\n内容</think>  结果  ":     "结果",
		"<think>未闭合的思考一直到结尾":                   "", // 未闭合 → 丢弃到结尾
		"正文<think>未闭合":                        "正文",
	}
	for in, want := range cases {
		if got := stripThink(in); got != want {
			t.Errorf("stripThink(%q)=%q, 期望 %q", in, got, want)
		}
	}
}
