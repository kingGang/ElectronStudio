// Package tools 提供可供大模型调用的工具注册与执行（function-calling 的执行端）。
//
// 设计为与具体业务解耦：工具的副作用（控制情绪、播放动作、操作设备等）以闭包形式
// 在注册时注入，因此本包不依赖 choreography / server 等，便于单测。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Handler 执行一次工具调用：argsJSON 为模型给出的参数（JSON 字符串），返回结果文本。
type Handler func(ctx context.Context, argsJSON string) (string, error)

// Spec 描述一个工具的声明（名称 / 说明 / JSON Schema 参数）。
type Spec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Tool 是声明 + 处理器。
type Tool struct {
	Spec    Spec
	Handler Handler
}

// Registry 是线程安全的工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册（或覆盖）一个工具。
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Spec.Name]; !ok {
		r.order = append(r.order, t.Spec.Name)
		sort.Strings(r.order) // 稳定顺序
	}
	r.tools[t.Spec.Name] = t
}

// Count 返回已注册工具数量。
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Specs 返回所有工具的声明（用于构造模型请求）。
func (r *Registry) Specs() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Spec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].Spec)
	}
	return out
}

// Execute 按名执行工具。未知工具或处理器报错都会返回 error。
func (r *Registry) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tools: 未知工具 %q", name)
	}
	return t.Handler(ctx, argsJSON)
}
