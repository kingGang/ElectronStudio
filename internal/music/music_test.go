package music

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestServiceSearchAndPlay 用 Mock 验证：搜索→解析→播放→状态变化回调。
func TestServiceSearchAndPlay(t *testing.T) {
	var gotState State
	var gotTrack Track
	svc := NewService(MockSearcher{}, NewMockPlayer(), nil, func(tr Track, st State) {
		gotTrack, gotState = tr, st
	})
	tr, err := svc.SearchAndPlay(context.Background(), "稻香")
	if err != nil {
		t.Fatalf("播放失败: %v", err)
	}
	if tr.Name != "稻香" || svc.State() != StatePlaying {
		t.Fatalf("结果错误: %+v state=%s", tr, svc.State())
	}
	if gotState != StatePlaying || gotTrack.Name != "稻香" {
		t.Fatalf("状态回调错误: %s %+v", gotState, gotTrack)
	}
	svc.Pause()
	if svc.State() != StatePaused {
		t.Fatal("暂停后状态应为 paused")
	}
}

// TestMpg123Commands 验证：mpg123 播放器把控制命令正确写入 stdin（注入伪 stdin，跳过真实子进程）。
func TestMpg123Commands(t *testing.T) {
	var buf bytes.Buffer
	p := NewMpg123Player("mpg123", nil)
	p.stdin = &buf
	p.state = StatePlaying

	_ = p.writeCmd("LOAD http://x/y.mp3")
	_ = p.Pause() // 写 PAUSE（不触发子进程启动）
	_ = p.writeCmd("VOLUME 30")

	out := buf.String()
	if !strings.Contains(out, "LOAD http://x/y.mp3") || !strings.Contains(out, "PAUSE") || !strings.Contains(out, "VOLUME 30") {
		t.Fatalf("命令写入错误: %q", out)
	}
	if p.State() != StatePaused {
		t.Fatalf("Pause 后状态应为 paused")
	}
}

// TestKuwoParse 验证酷我搜索响应的 JSON 解析（不依赖真实接口）。
func TestKuwoParse(t *testing.T) {
	var r kuwoSearchResp
	body := `{"data":{"list":[{"rid":123,"name":"稻香","artist":"周杰伦","duration":223}]}}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(r.Data.List) != 1 || r.Data.List[0].Name != "稻香" || r.Data.List[0].RID.String() != "123" {
		t.Fatalf("解析错误: %+v", r.Data.List)
	}
}
