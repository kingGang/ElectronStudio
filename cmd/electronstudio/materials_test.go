package main

import "testing"

// TestSanitizeMaterialName 验证情绪名净化：接受中文/字母/数字/_-（并转小写），
// 拒绝空、超长与任何含路径危险字符（. / \ : 空白 控制符）者——这是防路径穿越的关键。
func TestSanitizeMaterialName(t *testing.T) {
	valid := map[string]string{
		"happy":      "happy",
		"Dance":      "dance", // 转小写
		"a-b_1":      "a-b_1",
		"开心":         "开心",
		"跳舞2":        "跳舞2",
		"你好世界":       "你好世界",
		"  happy  ":  "happy", // 去首尾空白
	}
	for in, want := range valid {
		got, ok := sanitizeMaterialName(in)
		if !ok || got != want {
			t.Errorf("sanitize(%q)=(%q,%v)，期望 (%q,true)", in, got, ok, want)
		}
	}

	invalid := []string{
		"", "   ",
		"..", "../etc", "a/b", "a\\b", // 路径穿越
		"a.b",                // 含点（防扩展名/隐藏文件）
		"a b",                // 空格
		"c:", "<x>", "a|b",   // 其它危险/标点
		"name\x00",           // 控制符
		"这是一个超过二十四个汉字长度上限的中文情绪名称示例需要被拒绝掉哦", // 超长
	}
	for _, in := range invalid {
		if got, ok := sanitizeMaterialName(in); ok {
			t.Errorf("sanitize(%q)=%q 应被拒绝", in, got)
		}
	}
}
