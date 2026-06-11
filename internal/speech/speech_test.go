package speech

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestMockInject 验证 Mock 注入的事件能从 Events() 读到。
func TestMockInject(t *testing.T) {
	m := NewMock(nil)
	defer m.Close()

	m.Inject(Event{Kind: KindASR, Text: "你好", Final: true})

	select {
	case ev := <-m.Events():
		if ev.Kind != KindASR || ev.Text != "你好" || !ev.Final {
			t.Fatalf("事件内容错误: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到注入的事件")
	}
}

// TestSidecarReceiveAndSpeak 用一个假 sidecar 服务验证：
// 客户端能收到下行 asr 事件，并能把 speak 请求发回给 sidecar。
func TestSidecarReceiveAndSpeak(t *testing.T) {
	gotSpeak := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		// 1) 先推一条 asr 事件给客户端。
		asr, _ := json.Marshal(sidecarMsg{Type: "asr", Text: "打开台灯", Final: true})
		if err := conn.Write(ctx, websocket.MessageText, asr); err != nil {
			return
		}
		// 2) 读取客户端发来的 speak 请求并记录。
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m sidecarMsg
		_ = json.Unmarshal(data, &m)
		if m.Type == "speak" {
			gotSpeak <- m.Text
		}
		// 保持连接片刻，便于客户端完成读取。
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	sc := NewSidecar(wsURL, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sc.Start(ctx); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer sc.Close()

	// 应收到 asr 事件。
	select {
	case ev := <-sc.Events():
		if ev.Kind != KindASR || ev.Text != "打开台灯" {
			t.Fatalf("事件错误: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 asr 事件")
	}

	// 发送 speak，假服务端应收到。
	if err := sc.Speak(ctx, "好的"); err != nil {
		t.Fatalf("Speak 失败: %v", err)
	}
	select {
	case got := <-gotSpeak:
		if got != "好的" {
			t.Fatalf("speak 文本错误: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未收到 speak")
	}
}

// TestToEventUnknown 验证未知类型不会被误转为事件。
func TestToEventUnknown(t *testing.T) {
	if _, ok := toEvent(sidecarMsg{Type: "bogus"}); ok {
		t.Fatal("未知类型不应转换为事件")
	}
}
