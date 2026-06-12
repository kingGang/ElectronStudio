package llm

import (
	"context"
	"testing"
)

// fakeCompleter 模拟一个会话：第一次返回一个工具调用，第二次返回最终文本。
type fakeCompleter struct{ calls int }

func (f *fakeCompleter) Complete(_ context.Context, req Request) (Completion, error) {
	f.calls++
	if f.calls == 1 {
		return Completion{ToolCalls: []ToolCall{{ID: "c1", Name: "set_lamp", Arguments: `{"on":true}`}}}, nil
	}
	// 第二轮：历史里应已包含工具结果。
	return Completion{Content: "已为你开灯"}, nil
}

// TestRunToolLoop 验证工具循环：执行工具→回填→收敛到最终文本。
func TestRunToolLoop(t *testing.T) {
	var execed []string
	exec := func(_ context.Context, name, args string) (string, error) {
		execed = append(execed, name+":"+args)
		return "台灯已打开", nil
	}

	res, err := RunToolLoop(
		context.Background(), &fakeCompleter{},
		[]Message{{Role: RoleUser, Content: "开灯"}},
		[]Tool{{Name: "set_lamp"}},
		exec, 5, nil,
	)
	if err != nil {
		t.Fatalf("RunToolLoop 失败: %v", err)
	}
	if res.Content != "已为你开灯" {
		t.Fatalf("最终内容错误: %q", res.Content)
	}
	if len(res.Executed) != 1 || res.Executed[0].Name != "set_lamp" {
		t.Fatalf("执行记录错误: %+v", res.Executed)
	}
	if len(execed) != 1 || execed[0] != `set_lamp:{"on":true}` {
		t.Fatalf("工具未被正确执行: %v", execed)
	}
}

// TestRunToolLoopNoTools 验证：模型直接给文本（无工具调用）时立即返回。
func TestRunToolLoopNoTools(t *testing.T) {
	p := completerFunc(func(_ context.Context, _ Request) (Completion, error) {
		return Completion{Content: "你好"}, nil
	})
	res, err := RunToolLoop(context.Background(), p, nil, nil, nil, 5, nil)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.Content != "你好" || len(res.Executed) != 0 {
		t.Fatalf("结果错误: %+v", res)
	}
}

// completerFunc 把函数适配为 Completer。
type completerFunc func(context.Context, Request) (Completion, error)

func (f completerFunc) Complete(ctx context.Context, req Request) (Completion, error) {
	return f(ctx, req)
}
