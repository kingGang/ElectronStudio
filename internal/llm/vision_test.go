package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestVisionRequestEncodesImage 验证：Vision 把图片编码进 OpenAI 视觉格式（image_url 多模态消息）。
func TestVisionRequestEncodesImage(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"我看到一只猫"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAICompat(OpenAIConfig{ID: "v", Name: "vision", BaseURL: srv.URL, Model: "gpt-4o", Timeout: 3 * time.Second})
	r := NewRouter()
	r.Add(p)

	out, err := r.Vision(context.Background(), []byte{0xFF, 0xD8, 0xFF, 0xD9}, "这是什么")
	if err != nil {
		t.Fatalf("Vision 失败: %v", err)
	}
	if out != "我看到一只猫" {
		t.Fatalf("返回内容错误: %q", out)
	}
	if !strings.Contains(gotBody, "image_url") || !strings.Contains(gotBody, "data:image/jpeg;base64,") {
		t.Fatalf("请求未包含图片: %s", gotBody)
	}
	if !strings.Contains(gotBody, "这是什么") {
		t.Fatalf("请求未包含问题文本")
	}
}
