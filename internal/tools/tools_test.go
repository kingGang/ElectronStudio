package tools

import (
	"context"
	"testing"
)

// TestRegistryExecute 验证注册、列举与执行。
func TestRegistryExecute(t *testing.T) {
	r := NewRegistry()
	r.Register(TimeTool())
	if r.Count() != 1 {
		t.Fatalf("数量错误: %d", r.Count())
	}
	if specs := r.Specs(); len(specs) != 1 || specs[0].Name != "get_time" {
		t.Fatalf("声明错误: %+v", specs)
	}
	out, err := r.Execute(context.Background(), "get_time", "{}")
	if err != nil || out == "" {
		t.Fatalf("执行失败: out=%q err=%v", out, err)
	}
}

// TestRegistryUnknown 验证未知工具报错。
func TestRegistryUnknown(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Execute(context.Background(), "nope", "{}"); err == nil {
		t.Fatal("未知工具应报错")
	}
}

// TestLampTool 验证有状态设备工具：开/关改变状态。
func TestLampTool(t *testing.T) {
	lamp := NewLamp()
	tool := lamp.Tool()
	if _, err := tool.Handler(context.Background(), `{"on":true}`); err != nil {
		t.Fatalf("开灯失败: %v", err)
	}
	if !lamp.On() {
		t.Fatal("台灯状态应为开")
	}
	if _, err := tool.Handler(context.Background(), `{"on":false}`); err != nil {
		t.Fatalf("关灯失败: %v", err)
	}
	if lamp.On() {
		t.Fatal("台灯状态应为关")
	}
}

// TestLookTool 验证"看一眼"工具：抓帧 → 交给描述函数 → 返回描述。
func TestLookTool(t *testing.T) {
	captured := false
	tool := LookTool(
		func(_ context.Context) ([]byte, error) { captured = true; return []byte{1, 2, 3}, nil },
		func(_ context.Context, jpeg []byte, q string) (string, error) {
			if len(jpeg) == 0 || q == "" {
				t.Fatalf("描述函数入参错误: jpeg=%d q=%q", len(jpeg), q)
			}
			return "我看到一只猫", nil
		},
	)
	out, err := tool.Handler(context.Background(), `{"question":"这是什么动物"}`)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !captured || out != "我看到一只猫" {
		t.Fatalf("结果错误: captured=%v out=%q", captured, out)
	}
}

// TestEmotionTool 验证情绪工具会调用注入的回调。
func TestEmotionTool(t *testing.T) {
	var got string
	tool := EmotionTool([]string{"happy", "sad"}, func(e string) error { got = e; return nil })
	if _, err := tool.Handler(context.Background(), `{"emotion":"happy"}`); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got != "happy" {
		t.Fatalf("回调未收到情绪: %q", got)
	}
}
