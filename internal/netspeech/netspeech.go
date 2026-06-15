// Package netspeech 提供 OpenAI 兼容的网络语音能力：
//
//	TTS：POST {base_url}/audio/speech        请求 {model,input,voice,response_format} → 返回音频字节
//	ASR：POST {base_url}/audio/transcriptions  multipart 上传音频 → 返回 {text}
//
// 兼容 OpenAI 官方及各类 OpenAI 兼容网关/本地服务，凭 base_url/api_key/model 配置即可切换。
package netspeech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// TTSClient 是 OpenAI 兼容文字转语音客户端。
type TTSClient struct {
	baseURL string
	apiKey  string
	model   string
	voice   string
	format  string
	http    *http.Client
}

// NewTTS 创建网络 TTS 客户端。model/voice/format 为空时用合理默认值。
func NewTTS(baseURL, apiKey, model, voice, format string) *TTSClient {
	return &TTSClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   orStr(model, "tts-1"),
		voice:   orStr(voice, "alloy"),
		format:  orStr(format, "mp3"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Synthesize 合成一段语音，返回音频字节（格式由 format 决定，默认 mp3）。
// voiceOverride 非空时覆盖默认音色（用于"设备音色"运行时切换）。
func (c *TTSClient) Synthesize(ctx context.Context, text, voiceOverride string) ([]byte, error) {
	voice := c.voice
	if voiceOverride != "" {
		voice = voiceOverride
	}
	body, _ := json.Marshal(map[string]any{
		"model":           c.model,
		"input":           text,
		"voice":           voice,
		"response_format": c.format,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("netspeech: TTS 返回 %d: %s", resp.StatusCode, snippet(data))
	}
	return data, nil
}

// ASRClient 是 OpenAI 兼容语音识别客户端。
type ASRClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewASR 创建网络 ASR 客户端。model 为空时默认 whisper-1。
func NewASR(baseURL, apiKey, model string) *ASRClient {
	return &ASRClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   orStr(model, "whisper-1"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Transcribe 识别一段音频，返回文本。filename 用于让服务端识别格式（如 audio.webm/audio.wav）。
func (c *ASRClient) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", c.model)
	fw, err := w.CreateFormFile("file", orStr(filename, "audio.webm"))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	_ = w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("netspeech: ASR 返回 %d: %s", resp.StatusCode, snippet(data))
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("netspeech: 解析识别结果失败: %w (%s)", err, snippet(data))
	}
	return strings.TrimSpace(r.Text), nil
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
