package main

import "testing"

// TestParseMusicPlay 锁定"放歌"意图解析：确定性路径让小智等不支持工具的模型也能放歌。
func TestParseMusicPlay(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		match bool
	}{
		{"播放周杰伦的歌", "周杰伦", true},
		{"放一首晴天", "晴天", true},
		{"我想听陈奕迅", "陈奕迅", true},
		{"来首轻音乐", "轻", true}, // "轻音乐" 去掉尾"音乐"后剩"轻"
		{"放点音乐", "热门", true},
		{"随便来首歌", "热门", true},
		{"播放列表怎么做", "", false}, // 疑问句不劫持
		{"放假了真开心", "", false},   // 裸"放"不命中
		{"今天天气怎么样", "", false},
		{"你好啊", "", false},
	}
	for _, c := range cases {
		got, ok := parseMusicPlay(c.in)
		if ok != c.match {
			t.Errorf("parseMusicPlay(%q) 命中=%v，期望 %v", c.in, ok, c.match)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseMusicPlay(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}
