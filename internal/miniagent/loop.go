package miniagent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"log/slog"
)

// 单轮对话的 LLM 调用上限（默认值）：防止模型陷入工具循环烧 token，按典型
// 工具链（读→改→测→答）所需步骤数 + 适度余量设定。可经 LoopConfig.MaxIterations 覆盖。
const maxIterations = 20

// 单条 tool 结果进入历史的字符上限：超长输出会稀释上下文预算，2k 字符
// 既能保留工具结果的可读信息，又不至于挤占 LLM 的输入窗口。
const maxToolResultInHistory = 2000

// 同一步内并行工具的并发上限：防止 LLM 一次发起大量 tool_call 时耗尽
// FD/连接或触发目标服务限流。
const maxParallelTools = 8

func safeCall(ctx context.Context, logger *slog.Logger, tool Tool, name, args string) (res ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			if logger != nil {
				logger.Error("tool panic recovered", "tool", name, "panic", r)
			}
			res = ToolResult{IsError: true, Output: fmt.Sprintf("工具 %q 内部错误", name)}
		}
	}()
	return tool.Call(ctx, args)
}

// Run 单轮 ReAct 循环：把 userPrompt 发给 llm，模型若请求工具则执行后回灌，
// 直到模型给出无 tool_calls 的最终文本或撞 maxIterations 上限。
//
// onToolUse 仅在每次工具执行前被调用一次；最终文本只在返回的 Result.Text 里
// 一次性给出。logger 为 nil 时静默。
//
// Result.Messages 是截至返回时的全量 transcript，所有 return 路径均带回。
// 注意：onToolUse 报错返回时，Messages 尾部可能是"有 tool_calls、无对应
// tool 结果"的 assistant 消息——该尾部不能直接作为 History 续跑（端点会
// 因配对断裂 400）。CLI 在出错分支不写回 session，库使用者同理不应持久化。
func Run(ctx context.Context, llm *HTTPClient, cfg LoopConfig, userPrompt string, onToolUse OnToolUse, logger *slog.Logger) (Result, error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm client is nil")
	}
	toolByName := buildToolIndex(cfg.Tools, logger)

	// 复制 History 而非 append 到其底层数组：接续对话时调用方可能复用同一
	// slice，原地 append 会越界写调用方的数据。
	msgs := make([]Message, 0, len(cfg.History)+1)
	msgs = append(msgs, cfg.History...)
	msgs = append(msgs, Message{Role: roleUser, Content: userPrompt})
	total := Usage{}
	// 调用方可经 cfg.MaxIterations 覆盖默认上限；<=0 回退到包默认 maxIterations。
	iterLimit := cfg.MaxIterations
	if iterLimit <= 0 {
		iterLimit = maxIterations
	}

	for step := 1; step <= iterLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs}, err
		}
		resp, err := callLLM(ctx, llm, cfg, step, msgs, logger)
		if errors.Is(err, ErrContextLength) {
			// 撞端点 context 上限：收紧历史（先清 reasoning，再压 tool content）后
			// 对本步重试一次；仍超则由下方 err 分支上抛。只降级一次，避免循环烧请求。
			msgs = trimHistoryForContext(msgs)
			if logger != nil {
				logger.Warn("context length exceeded; trimmed history; retrying step", "step", step)
			}
			resp, err = callLLM(ctx, llm, cfg, step, msgs, logger)
		}
		if err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs}, err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		// 预算熔断：以端点返回的真实 usage 累计判定，超限即停（error 路径 + 退出码 1）。
		if cfg.MaxTotalTokens > 0 && total.InputTokens+total.OutputTokens > cfg.MaxTotalTokens {
			return Result{Usage: total, Steps: step, Messages: msgs}, fmt.Errorf(
				"%w: input=%d output=%d（累计超 MaxTotalTokens %d）",
				ErrBudgetExceeded, total.InputTokens, total.OutputTokens, cfg.MaxTotalTokens,
			)
		}

		if len(resp.ToolCalls) == 0 {
			// 最终文本入历史：接续对话需要看到上一轮的回答。
			msgs = append(msgs, Message{Role: roleAssistant, Content: resp.Text, Reasoning: resp.Reasoning})
			return Result{Text: resp.Text, Usage: total, Steps: step, Finish: finishStop, Messages: msgs}, nil
		}

		msgs, err = handleToolCalls(ctx, step, resp, toolByName, msgs, onToolUse, logger)
		if err != nil {
			return Result{Usage: total, Steps: step, Messages: msgs}, err
		}
	}
	// 达到迭代上限：返回 nil error，让上层仍能消费已累积的 Usage。
	// Finish=finishMaxIterations 是终止信号（无最终 Text）。
	return Result{Usage: total, Steps: iterLimit, Finish: finishMaxIterations, Messages: msgs}, nil
}

func buildToolIndex(tools []Tool, logger *slog.Logger) map[string]Tool {
	toolByName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := toolByName[t.Name]; dup && logger != nil {
			// 重名静默覆盖会让前者不可达且无任何线索，路由歧义极难排查。
			logger.Warn("duplicate tool name, last wins", "tool", t.Name)
		}
		toolByName[t.Name] = t
	}
	return toolByName
}

func callLLM(ctx context.Context, llm *HTTPClient, cfg LoopConfig, step int, msgs []Message, logger *slog.Logger) (Response, error) {
	if logger != nil {
		logger.Debug("llm call start", "step", step, "model", cfg.Model)
	}
	req := Request{
		Model:     cfg.Model,
		System:    cfg.System,
		Messages:  msgs,
		MaxTokens: cfg.MaxTokens,
		Tools:     cfg.Tools,
	}
	resp, err := llm.Do(ctx, req)
	if err != nil {
		if logger != nil {
			logger.Warn("llm call failed", "step", step, "error", err)
		}
		return Response{}, fmt.Errorf("llm call %d: %w", step, err)
	}
	if logger != nil {
		logger.Info("llm call done", "step", step, "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "tool_calls", len(resp.ToolCalls), "finish_reason", resp.FinishReason)
	}
	// finish_reason 非 stop/tool_calls 表示回答被 max_tokens 或内容过滤截断。
	if logger != nil && resp.FinishReason != "" && resp.FinishReason != "stop" && resp.FinishReason != "tool_calls" {
		logger.Warn("llm response truncated", "step", step, "finish_reason", resp.FinishReason)
	}
	return resp, nil
}

func handleToolCalls(ctx context.Context, step int, resp Response, toolByName map[string]Tool, msgs []Message, onToolUse OnToolUse, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	// 思考链随 assistant 消息入历史（回灌 reasoning 模型所需）。
	msgs = append(msgs, Message{Role: roleAssistant, Reasoning: resp.Reasoning, ToolCalls: calls})

	// 先按序通知本轮全部 tool_use：消费方尽早看到完整工具计划，且顺序确定。
	if onToolUse != nil {
		for _, tc := range calls {
			if err := onToolUse(tc.Name, tc.Args); err != nil {
				return msgs, err
			}
		}
	}

	// 同一步内 LLM 一次发起的多个 tool_call 相互独立，串行会让总耗时 = Σ 单工具
	// 耗时（shell 可达数十秒）。并行执行，结果按原 index 回填，保证历史消息
	// 与 assistant.tool_calls 一一对应（OpenAI 要求顺序匹配）。
	results := runToolsParallel(ctx, logger, calls, toolByName)

	for i, tc := range calls {
		tres := results[i]
		if logger != nil {
			logger.Info("tool executed", "step", step, "tool", tc.Name, "is_error", tres.IsError, "output_len", len(tres.Output))
		}
		limit := 0
		if t, ok := toolByName[tc.Name]; ok {
			limit = t.ResultLimit
		}
		msgs = append(msgs, Message{Role: roleTool, ToolCallID: tc.ID, Content: trimForHistory(tres.Output, limit)})
	}
	return msgs, nil
}

// runToolsParallel 并行执行 calls，返回与 calls 同序的结果。
// 各 goroutine 写入 results 的不同下标，无内存竞争；wg.Wait 提供 happens-before。
// 未知工具在调度前短路，直接回填错误结果。每个 tool 的 panic 由 safeCall 兜底。
// 用 buffered chan 做信号量限制同时在途的工具数（maxParallelTools）。
func runToolsParallel(ctx context.Context, logger *slog.Logger, calls []ToolCall, toolByName map[string]Tool) []ToolResult {
	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelTools)
	for i, tc := range calls {
		tool, ok := toolByName[tc.Name]
		if !ok {
			results[i] = ToolResult{IsError: true, Output: fmt.Sprintf("未知工具 %q", tc.Name)}
			continue
		}
		wg.Add(1)
		go func(i int, tc ToolCall, tool Tool) {
			defer wg.Done()
			// 信号量获取联动 ctx：取消后排队中的调用直接放弃，不再等空位；
			// 否则一个不尊重 ctx 的阻塞工具会永久占位，wg.Wait 不返回、Run 挂死。
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = ToolResult{IsError: true, Output: "已取消"}
				return
			}
			defer func() { <-sem }()
			results[i] = safeCall(ctx, logger, tool, tc.Name, tc.Args)
		}(i, tc, tool)
	}
	wg.Wait()
	return results
}

// trimForHistory 把工具结果裁到 limit 字符后入历史；limit<=0 用默认上限。
// read/edit 等代码类工具经 Tool.ResultLimit 传高限，避免截断丢准确性。
// C-2 的 context 降级复用同一裁剪语义（对更早的 tool content 用更小 limit 再裁）。
func trimForHistory(s string, limit int) string {
	if limit <= 0 {
		limit = maxToolResultInHistory
	}
	return truncate(s, limit, "…[tool_result 已截断]")
}
