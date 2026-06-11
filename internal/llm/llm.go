// Package llm 提供大模型的统一抽象与多模型路由。
//
// 设计目标（对应需求 6「大模型可以配置任意多个」）：
//   - Provider 接口屏蔽不同厂商差异，统一为「输入消息 → 流式输出增量」；
//   - Router 持有任意多个 Provider，可运行时切换当前生效模型；
//   - 流式输出用 channel 表达，便于把增量直接转成前端的 chat 流式事件。
//
// 本文件只含纯逻辑与接口；具体实现见 echo.go（本地假模型）与
// openai.go（OpenAI 兼容 / Ollama）。
package llm

import (
	"context"
	"fmt"
	"sync"
)

// Role 是对话消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message 是一条对话消息。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Request 是一次对话请求。
type Request struct {
	Messages    []Message `json:"messages"`
	Temperature float32   `json:"temperature,omitempty"`
}

// Chunk 是流式输出中的一个增量片段。
// 约定：正常增量携带 Delta；出错时携带 Err；结束时 Done=true。
// 消费者应一直读取 channel 直到其被关闭。
type Chunk struct {
	Delta string // 本次新增的文本
	Done  bool   // 是否为结束标志
	Err   error  // 非 nil 表示发生错误（随后 channel 关闭）
}

// Info 描述一个模型的元信息。
type Info struct {
	ID       string `json:"id"`       // 唯一标识，用于切换
	Name     string `json:"name"`     // 展示名
	Provider string `json:"provider"` // 提供方，如 ollama / openai / anthropic
}

// Provider 抽象一个具体的大模型后端。
type Provider interface {
	// Info 返回模型元信息。
	Info() Info
	// Chat 发起一次对话，返回流式增量 channel。
	// 实现应在完成或出错后关闭 channel，并尊重 ctx 取消。
	Chat(ctx context.Context, req Request) (<-chan Chunk, error)
}

// Router 管理多个 Provider，并维护"当前生效"模型。并发安全。
type Router struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string // 保持添加顺序，便于稳定展示
	active    string
}

// NewRouter 创建一个空路由器。
func NewRouter() *Router {
	return &Router{providers: make(map[string]Provider)}
}

// Add 注册一个 Provider。首个被添加的 Provider 自动成为当前生效模型。
// 重复 ID 会覆盖原有 Provider。
func (r *Router) Add(p Provider) {
	id := p.Info().ID
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[id]; !exists {
		r.order = append(r.order, id)
	}
	r.providers[id] = p
	if r.active == "" {
		r.active = id
	}
}

// List 按添加顺序返回所有模型的元信息。
func (r *Router) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.providers[id].Info())
	}
	return out
}

// ActiveID 返回当前生效模型的 ID（无任何模型时为空字符串）。
func (r *Router) ActiveID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// SetActive 切换当前生效模型。
func (r *Router) SetActive(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[id]; !ok {
		return fmt.Errorf("llm: 未知模型 %q", id)
	}
	r.active = id
	return nil
}

// Chat 使用当前生效模型发起对话。
func (r *Router) Chat(ctx context.Context, req Request) (<-chan Chunk, error) {
	r.mu.RLock()
	p, ok := r.providers[r.active]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: 无可用模型")
	}
	return p.Chat(ctx, req)
}
