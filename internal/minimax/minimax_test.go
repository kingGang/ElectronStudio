package minimax

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSynthesize 用假服务器验证：请求字段正确、hex 音频被正确解码、status_code!=0 报错。
func TestSynthesize(t *testing.T) {
	want := []byte("FAKE-MP3-BYTES")
	var gotAuth, gotPath string
	var gotReq t2aRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		resp := t2aResponse{}
		resp.Data.Audio = hex.EncodeToString(want)
		resp.BaseResp = baseResp{StatusCode: 0, StatusMsg: "success"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "k123")
	got, err := c.Synthesize(context.Background(), "你好", SpeakOptions{VoiceID: "female-yujie"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("音频解码错误: %q", got)
	}
	if gotAuth != "Bearer k123" {
		t.Fatalf("鉴权头错误: %q", gotAuth)
	}
	if gotPath != "/t2a_v2" {
		t.Fatalf("端点错误: %q", gotPath)
	}
	if gotReq.VoiceSetting.VoiceID != "female-yujie" || gotReq.OutputFormat != "hex" || gotReq.Model != DefaultTTSModel {
		t.Fatalf("请求字段错误: %+v", gotReq)
	}
}

// TestSynthesizeBizError 验证 HTTP 200 但 status_code!=0 时报错。
func TestSynthesizeBizError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := t2aResponse{}
		resp.BaseResp = baseResp{StatusCode: 1004, StatusMsg: "invalid api key"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	if _, err := c.Synthesize(context.Background(), "x", SpeakOptions{}); err == nil {
		t.Fatal("status_code!=0 应报错")
	}
}

// TestGenerateImage 验证：解析 image_urls，并下载该 URL 内容为字节。
func TestGenerateImage(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nFAKE")
	mux := http.NewServeMux()
	mux.HandleFunc("/image_generation", func(w http.ResponseWriter, r *http.Request) {
		var req imageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != DefaultImageModel || req.ResponseFormat != "url" {
			t.Errorf("图片请求字段错误: %+v", req)
		}
		resp := imageResponse{}
		// 指回本服务器的 /img 作为下载地址。
		resp.Data.ImageURLs = []string{"http://" + r.Host + "/img"}
		resp.BaseResp = baseResp{StatusCode: 0}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/img", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(png) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "k")
	got, err := c.GenerateImage(context.Background(), "一只猫", ImageOptions{})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if string(got) != string(png) {
		t.Fatalf("下载图片字节不符: %q", got)
	}
}
