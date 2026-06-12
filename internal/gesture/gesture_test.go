package gesture

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

// TestMockInject 验证 Mock 注入的手势能从 Events() 读到。
func TestMockInject(t *testing.T) {
	m := NewMock(nil)
	defer m.Close()
	m.Inject(Event{Name: "wave", Confidence: 0.9})
	select {
	case ev := <-m.Events():
		if ev.Name != "wave" {
			t.Fatalf("手势错误: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到手势")
	}
}

// TestSidecarReceivesGesture 用假 sidecar 验证：客户端能收到手势事件。
func TestSidecarReceivesGesture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		msg, _ := json.Marshal(sidecarMsg{Type: "gesture", Name: "thumbs_up", Confidence: 0.8})
		_ = c.Write(r.Context(), websocket.MessageText, msg)
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	sc := NewSidecar("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sc.Start(ctx); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer sc.Close()

	select {
	case ev := <-sc.Events():
		if ev.Name != "thumbs_up" {
			t.Fatalf("手势错误: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到手势事件")
	}
}
