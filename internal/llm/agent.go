package llm

import "context"

// Completer 是 RunToolLoop 所需的最小能力：发起一次非流式对话。
// Router 与单个 Provider 均满足此接口。
type Completer interface {
	Complete(ctx context.Context, req Request) (Completion, error)
}

// Executor 执行一次工具调用，返回结果文本。由上层（设备/编排层）提供。
type Executor func(ctx context.Context, name, argsJSON string) (string, error)

// ExecutedCall 记录一次已执行的工具调用及其结果（用于回传前端展示）。
type ExecutedCall struct {
	ID        string
	Name      string
	Arguments string
	Result    string
	Err       string // 非空表示执行出错
}

// Result 是一次工具调用循环的最终结果。
type Result struct {
	Content  string         // 最终回复文本
	Executed []ExecutedCall // 过程中执行的全部工具调用（按顺序）
}

// RunToolLoop 驱动 function-calling 多轮循环：
//
//	while 模型返回工具调用:
//	    执行每个工具 → 把结果作为 tool 消息回填 → 再次请求
//	直到模型不再调用工具，返回其文本。
//
// onCall 可选，在每次工具执行后回调（用于实时把工具进度推给前端）。
// maxRounds 限制循环轮数，防止模型反复调用导致死循环。
func RunToolLoop(
	ctx context.Context,
	p Completer,
	msgs []Message,
	tools []Tool,
	exec Executor,
	maxRounds int,
	onCall func(ExecutedCall),
) (Result, error) {
	if maxRounds <= 0 {
		maxRounds = 5
	}
	var executed []ExecutedCall

	for round := 0; round < maxRounds; round++ {
		comp, err := p.Complete(ctx, Request{Messages: msgs, Tools: tools})
		if err != nil {
			return Result{}, err
		}
		// 无工具调用 → 收敛，返回最终文本。
		if len(comp.ToolCalls) == 0 {
			return Result{Content: comp.Content, Executed: executed}, nil
		}

		// 把"助手发起的工具调用"作为一条消息加入历史。
		msgs = append(msgs, Message{
			Role:      RoleAssistant,
			Content:   comp.Content,
			ToolCalls: comp.ToolCalls,
		})

		// 逐个执行工具，并把结果作为 tool 消息回填。
		for _, call := range comp.ToolCalls {
			result, execErr := exec(ctx, call.Name, call.Arguments)
			ec := ExecutedCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments, Result: result}
			if execErr != nil {
				ec.Err = execErr.Error()
				result = "工具执行失败: " + execErr.Error()
			}
			executed = append(executed, ec)
			if onCall != nil {
				onCall(ec)
			}
			msgs = append(msgs, Message{
				Role:       RoleTool,
				Content:    result,
				ToolCallID: call.ID,
				Name:       call.Name,
			})
		}
	}

	// 达到轮数上限：返回已有信息，避免无限循环。
	return Result{Content: "（已达到工具调用轮数上限）", Executed: executed}, nil
}
