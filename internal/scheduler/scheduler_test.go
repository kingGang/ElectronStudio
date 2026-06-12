package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestNextFire 验证三种触发方式的下次时间计算。
func TestNextFire(t *testing.T) {
	from := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	// Every。
	n, err := nextFire(Job{Every: "30m"}, from)
	if err != nil || !n.Equal(from.Add(30*time.Minute)) {
		t.Fatalf("Every 计算错误: %v %v", n, err)
	}
	// Daily 今天已过 → 明天。
	n, err = nextFire(Job{Daily: "08:00"}, from)
	if err != nil || n.Hour() != 8 || !n.After(from) {
		t.Fatalf("Daily 计算错误: %v %v", n, err)
	}
	// 非法。
	if _, err := nextFire(Job{}, from); err == nil {
		t.Fatal("缺触发时间应报错")
	}
	if _, err := nextFire(Job{Daily: "99:99"}, from); err == nil {
		t.Fatal("非法每日时间应报错")
	}
}

// TestFireOneShot 验证一次性任务到点触发且随后被移除。
func TestFireOneShot(t *testing.T) {
	fired := make(chan Job, 1)
	s := New(func(j Job) { fired <- j }, nil)
	s.tick = 20 * time.Millisecond

	at := time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	id, err := s.Add(Job{Title: "喝水", At: at, Action: Action{Kind: "say", Text: "该喝水了"}})
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case j := <-fired:
		if j.Action.Text != "该喝水了" {
			t.Fatalf("触发内容错误: %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("任务未触发")
	}

	// 一次性任务触发后应被移除。
	time.Sleep(60 * time.Millisecond)
	for _, j := range s.List() {
		if j.ID == id {
			t.Fatal("一次性任务触发后未移除")
		}
	}
}
