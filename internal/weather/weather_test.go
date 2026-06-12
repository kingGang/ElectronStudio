package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGet 用假服务端验证天气查询返回去除空白的文本。
func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("北京: ☀️ +25°C\n"))
	}))
	defer srv.Close()

	c := New()
	c.baseURL = srv.URL
	out, err := c.Get(context.Background(), "Beijing")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if out != "北京: ☀️ +25°C" {
		t.Fatalf("结果错误: %q", out)
	}
}
