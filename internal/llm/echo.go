package llm

import (
	"context"
	"strings"
	"time"
)

// Echo 是一个本地"假模型"，不联网，把用户最后一句话以「收到：…」的形式
// 逐字流式回显。用于无网络/无 API key 时跑通整条链路与前端联调。
type Echo struct {
	info Info
	// perTokenDelay 控制每个增量之间的间隔，模拟真实流式输出的节奏。
	perTokenDelay time.Duration
}

// NewEcho 创建一个 Echo Provider。
func NewEcho(id, name string) *Echo {
	return &Echo{
		info:          Info{ID: id, Name: name, Provider: "echo"},
		perTokenDelay: 20 * time.Millisecond,
	}
}

// Info 实现 Provider。
func (e *Echo) Info() Info { return e.info }

// Chat 实现 Provider：把回复按字符流式发送。
func (e *Echo) Chat(ctx context.Context, req Request) (<-chan Chunk, error) {
	reply := "收到：" + lastUserMessage(req)

	out := make(chan Chunk)
	go func() {
		defer close(out)
		for _, r := range reply {
			select {
			case <-ctx.Done():
				// 尊重取消：发出错误并结束。
				out <- Chunk{Err: ctx.Err()}
				return
			case out <- Chunk{Delta: string(r)}:
			}
			if e.perTokenDelay > 0 {
				select {
				case <-ctx.Done():
					out <- Chunk{Err: ctx.Err()}
					return
				case <-time.After(e.perTokenDelay):
				}
			}
		}
		out <- Chunk{Done: true}
	}()
	return out, nil
}

// Complete 实现 Provider：非流式返回回显内容；Echo 不发起工具调用。
func (e *Echo) Complete(_ context.Context, req Request) (Completion, error) {
	return Completion{Content: "收到：" + lastUserMessage(req)}, nil
}

// SupportsTools 实现 Provider：Echo 不支持工具调用。
func (e *Echo) SupportsTools() bool { return false }

// lastUserMessage 取请求中最后一条用户消息内容；没有则返回空串。
func lastUserMessage(req Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			return strings.TrimSpace(req.Messages[i].Content)
		}
	}
	return ""
}
