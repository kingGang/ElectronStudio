package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat 是兼容 OpenAI Chat Completions 协议的 Provider。
// 同一实现可对接：OpenAI、Ollama（/v1）、以及大量自托管的 OpenAI 兼容服务。
//
// 它通过 SSE（Server-Sent Events）消费流式响应，与本包的 Chunk 模型对接。
type OpenAICompat struct {
	info    Info
	baseURL string // 形如 https://api.openai.com/v1 或 http://localhost:11434/v1
	apiKey  string // 可为空（如本地 Ollama）
	model   string // 模型名，如 gpt-4o / qwen2.5
	client  *http.Client
}

// OpenAIConfig 配置一个 OpenAICompat Provider。
type OpenAIConfig struct {
	ID      string // 路由内唯一标识；为空时回退为 "openai:"+Model
	Name    string // 展示名；为空时回退为 Model
	BaseURL string // API 根地址（不含末尾斜杠）
	APIKey  string // 鉴权密钥（可空）
	Model   string // 模型名
	// Timeout 为单次请求的整体超时；为 0 时使用默认 60s。
	Timeout time.Duration
}

// NewOpenAICompat 创建一个 OpenAI 兼容 Provider。
func NewOpenAICompat(cfg OpenAIConfig) *OpenAICompat {
	id := cfg.ID
	if id == "" {
		id = "openai:" + cfg.Model
	}
	name := cfg.Name
	if name == "" {
		name = cfg.Model
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &OpenAICompat{
		info:    Info{ID: id, Name: name, Provider: "openai"},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		client:  &http.Client{Timeout: timeout},
	}
}

// Info 实现 Provider。
func (p *OpenAICompat) Info() Info { return p.info }

// chatRequestBody 是发送给 /chat/completions 的请求体（仅含本项目用到的字段）。
type chatRequestBody struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature float32   `json:"temperature,omitempty"`
}

// streamChunk 是 SSE 中每个 data 行的 JSON 结构（仅取所需字段）。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// Chat 实现 Provider：发起流式请求并把增量转成 Chunk。
func (p *OpenAICompat) Chat(ctx context.Context, req Request) (<-chan Chunk, error) {
	body, err := json.Marshal(chatRequestBody{
		Model:       p.model,
		Messages:    req.Messages,
		Stream:      true,
		Temperature: req.Temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: 序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: 请求失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 读取少量错误体用于诊断，随后关闭。
		snippet := readSnippet(resp.Body, 512)
		resp.Body.Close()
		return nil, fmt.Errorf("llm: 服务返回 %d: %s", resp.StatusCode, snippet)
	}

	out := make(chan Chunk)
	go p.stream(ctx, resp, out)
	return out, nil
}

// stream 解析 SSE 响应并把增量推入 out，结束/出错后关闭 out。
func (p *OpenAICompat) stream(ctx context.Context, resp *http.Response, out chan<- Chunk) {
	defer close(out)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// 放宽单行上限，避免长 token 行被截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // 忽略空行与非 data 行（如注释/事件名）
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			emit(ctx, out, Chunk{Done: true})
			return
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单行解析失败不致命，跳过即可。
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				if !emit(ctx, out, Chunk{Delta: ch.Delta.Content}) {
					return // ctx 取消
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		emit(ctx, out, Chunk{Err: fmt.Errorf("llm: 读取流失败: %w", err)})
		return
	}
	// 流自然结束但未见 [DONE]，补一个结束标志。
	emit(ctx, out, Chunk{Done: true})
}

// emit 在尊重 ctx 取消的前提下发送一个 Chunk，返回 false 表示已取消。
func emit(ctx context.Context, out chan<- Chunk, c Chunk) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- c:
		return true
	}
}

// readSnippet 从 r 读取至多 n 字节用于错误诊断。
func readSnippet(r interface{ Read([]byte) (int, error) }, n int) string {
	buf := make([]byte, n)
	read, _ := r.Read(buf)
	return string(buf[:read])
}
