package llm

import (
	"context"
	"strings"
	"testing"
	"time"
)

// drain 消费一个 Chunk 流，拼接全部 Delta 并返回，遇到错误时报告。
func drain(t *testing.T, ch <-chan Chunk) string {
	t.Helper()
	var sb strings.Builder
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("流出错: %v", c.Err)
		}
		sb.WriteString(c.Delta)
	}
	return sb.String()
}

// TestEchoChat 验证 Echo Provider 的流式回显内容。
func TestEchoChat(t *testing.T) {
	e := NewEcho("echo", "本地回声")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := e.Chat(ctx, Request{Messages: []Message{
		{Role: RoleUser, Content: "你好"},
	}})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	got := drain(t, ch)
	if got != "收到：你好" {
		t.Fatalf("回显内容错误: %q", got)
	}
}

// TestRouterAddAndActive 验证路由器的添加、列举与默认生效模型。
func TestRouterAddAndActive(t *testing.T) {
	r := NewRouter()
	if r.ActiveID() != "" {
		t.Fatal("空路由器不应有生效模型")
	}
	r.Add(NewEcho("a", "A"))
	r.Add(NewEcho("b", "B"))

	if r.ActiveID() != "a" {
		t.Fatalf("首个添加应为默认生效, 实际 %q", r.ActiveID())
	}
	if list := r.List(); len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("列举顺序错误: %+v", list)
	}
}

// TestRouterSetActive 验证切换生效模型及非法切换报错。
func TestRouterSetActive(t *testing.T) {
	r := NewRouter()
	r.Add(NewEcho("a", "A"))
	r.Add(NewEcho("b", "B"))

	if err := r.SetActive("b"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	if r.ActiveID() != "b" {
		t.Fatalf("切换后应为 b, 实际 %q", r.ActiveID())
	}
	if err := r.SetActive("missing"); err == nil {
		t.Fatal("切换到未知模型应报错")
	}
}

// TestRouterChatUsesActive 验证 Router.Chat 走当前生效模型。
func TestRouterChatUsesActive(t *testing.T) {
	r := NewRouter()
	r.Add(NewEcho("a", "A"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := r.Chat(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if got := drain(t, ch); got != "收到：x" {
		t.Fatalf("内容错误: %q", got)
	}
}
