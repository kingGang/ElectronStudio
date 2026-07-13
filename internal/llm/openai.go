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
// 流式对话（Chat）用 SSE 消费；工具调用（Complete）用非流式一次性返回。
type OpenAICompat struct {
	info    Info
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// OpenAIConfig 配置一个 OpenAICompat Provider。
type OpenAIConfig struct {
	ID      string
	Name    string
	BaseURL string
	APIKey  string
	Model   string
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
	// 【不要用 http.Client.Timeout】——它是"整个请求含读完响应体"的硬死线，而对话是流式 SSE：
	// 响应体边生成边读，一旦模型说得久一点（推理模型、长回答、工具多轮）就会在说到一半时被我们
	// 自己掐断、报"超时"。正确的约束是【首字节】：服务器迟迟不开口才算超时，开口之后爱说多久说多久。
	// 整轮的取消交给 ctx（新一轮对话会 cancel 上一轮，见 handleChat 的 turnCancel）。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = timeout // 首字节超时
	return &OpenAICompat{
		info:    Info{ID: id, Name: name, Provider: "openai"},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		client:  &http.Client{Transport: tr},
	}
}

// Info 实现 Provider。
func (p *OpenAICompat) Info() Info { return p.info }

// SupportsTools 实现 Provider：OpenAI 兼容协议支持 function-calling。
func (p *OpenAICompat) SupportsTools() bool { return true }

// ---- 线上消息格式（OpenAI Chat Completions）----

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // 固定 "function"
	Function oaFunc `json:"function"`
}
type oaFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // string，或含图片时为 []oaPart
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

// oaPart 是多模态消息内容的一个片段（文本或图片）。
type oaPart struct {
	Type     string      `json:"type"` // "text" | "image_url"
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}
type oaImageURL struct {
	URL string `json:"url"`
}
type oaTool struct {
	Type     string     `json:"type"` // 固定 "function"
	Function oaToolDecl `json:"function"`
}
type oaToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}
type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []oaTool    `json:"tools,omitempty"`
	ToolChoice  string      `json:"tool_choice,omitempty"`
	Stream      bool        `json:"stream"`
	Temperature float32     `json:"temperature,omitempty"`
}

// toWireMessages 把内部 Message 转换为 OpenAI 线上格式（含工具调用编码）。
func toWireMessages(msgs []Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	for _, m := range msgs {
		om := oaMessage{Role: string(m.Role), Content: m.Content}
		// 含图片时，content 改为"文本 + 图片"多模态数组（OpenAI 视觉格式）。
		if len(m.Images) > 0 {
			parts := []oaPart{{Type: "text", Text: m.Content}}
			for _, img := range m.Images {
				parts = append(parts, oaPart{Type: "image_url", ImageURL: &oaImageURL{URL: img}})
			}
			om.Content = parts
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID: tc.ID, Type: "function",
				Function: oaFunc{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		om.ToolCallID = m.ToolCallID
		om.Name = m.Name
		out = append(out, om)
	}
	return out
}

// toWireTools 把内部 Tool 转换为 OpenAI 线上格式。
func toWireTools(tools []Tool) []oaTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]oaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, oaTool{
			Type:     "function",
			Function: oaToolDecl{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return out
}

func (p *OpenAICompat) buildRequest(req Request, stream bool) oaRequest {
	body := oaRequest{
		Model:       p.model,
		Messages:    toWireMessages(req.Messages),
		Tools:       toWireTools(req.Tools),
		Stream:      stream,
		Temperature: req.Temperature,
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}
	return body
}

// post 发送请求并返回响应（调用方负责关闭 Body）。
func (p *OpenAICompat) post(ctx context.Context, body oaRequest) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: 序列化请求失败: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if body.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: 请求失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := readSnippet(resp.Body, 512)
		resp.Body.Close()
		return nil, fmt.Errorf("llm: 服务返回 %d: %s", resp.StatusCode, snippet)
	}
	return resp, nil
}

// Complete 实现 Provider：非流式请求，返回内容与（可能的）工具调用。
func (p *OpenAICompat) Complete(ctx context.Context, req Request) (Completion, error) {
	resp, err := p.post(ctx, p.buildRequest(req, false))
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string       `json:"content"`
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Completion{}, fmt.Errorf("llm: 解析响应失败: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Completion{}, fmt.Errorf("llm: 响应无 choices")
	}
	msg := parsed.Choices[0].Message
	comp := Completion{Content: msg.Content}
	for _, tc := range msg.ToolCalls {
		comp.ToolCalls = append(comp.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	return comp, nil
}

// streamChunk 是 SSE 中每个 data 行的 JSON 结构（仅取所需字段）。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// Chat 实现 Provider：发起流式请求并把增量转成 Chunk。
func (p *OpenAICompat) Chat(ctx context.Context, req Request) (<-chan Chunk, error) {
	// 兜底上限：首字节有 ResponseHeaderTimeout 管着，但服务器"开了口又中途哑掉"时读会一直阻塞。
	// 给整条流一个宽松的上限（远大于任何正常回答），只用来防挂死，正常永不命中。
	ctx, cancel := context.WithTimeout(ctx, streamMaxDuration)
	resp, err := p.post(ctx, p.buildRequest(req, true))
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan Chunk)
	go func() {
		defer cancel()
		p.stream(ctx, resp, out)
	}()
	return out, nil
}

// streamMaxDuration 是单条流式回答的兜底上限（防服务器中途哑掉导致读阻塞）。
const streamMaxDuration = 10 * time.Minute

// stream 解析 SSE 响应并把增量推入 out，结束/出错后关闭 out。
func (p *OpenAICompat) stream(ctx context.Context, resp *http.Response, out chan<- Chunk) {
	defer close(out)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			emit(ctx, out, Chunk{Done: true})
			return
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				if !emit(ctx, out, Chunk{Delta: ch.Delta.Content}) {
					return
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		emit(ctx, out, Chunk{Err: fmt.Errorf("llm: 读取流失败: %w", err)})
		return
	}
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
