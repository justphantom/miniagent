package miniagent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"log/slog"
)

// 单轮对话的 LLM 调用上限：防止模型陷入工具循环烧 token，按典型工具链
// （读→改→测→答）所需步骤数 + 适度余量设定。
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
// emit 仅在每次工具调用前发 SignalToolUse；最终文本只在返回的 Result.Text 里
// 一次性给出，不再做增量透传。logger 为 nil 时静默。
func Run(ctx context.Context, llm *HTTPClient, cfg LoopConfig, userPrompt string, emit EmitFunc, logger *slog.Logger) (Result, error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm client is nil")
	}
	toolByName := buildToolIndex(cfg.Tools)
	emitSignal := func(sig Signal) error {
		if emit != nil {
			return emit(sig)
		}
		return nil
	}

	msgs := []Message{{Role: "user", Content: userPrompt}}
	total := Usage{}

	for step := 1; step <= maxIterations; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1}, err
		}
		resp, err := callLLM(ctx, llm, cfg, step, msgs, logger)
		if err != nil {
			return Result{Usage: total, Steps: step - 1}, err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens

		if len(resp.ToolCalls) == 0 {
			return Result{Text: resp.Text, Usage: total, Steps: step}, nil
		}

		msgs, err = handleToolCalls(ctx, step, resp, toolByName, msgs, emitSignal, logger)
		if err != nil {
			return Result{Usage: total, Steps: step}, err
		}
	}
	// 达到迭代上限：返回 nil error，让上层仍能消费已累积的 Usage。
	// Steps=maxIterations 是终止信号（无最终 Text）。
	return Result{Usage: total, Steps: maxIterations}, nil
}

func buildToolIndex(tools []Tool) map[string]Tool {
	toolByName := make(map[string]Tool, len(tools))
	for _, t := range tools {
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

func handleToolCalls(ctx context.Context, step int, resp Response, toolByName map[string]Tool, msgs []Message, emitSignal func(Signal) error, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	msgs = append(msgs, Message{Role: "assistant", ToolCalls: calls})

	// 先按序通知本轮全部 tool_use：消费方尽早看到完整工具计划，且 emit 顺序确定。
	for _, tc := range calls {
		if err := emitSignal(Signal{Kind: SignalToolUse, Name: tc.Name, Input: tc.Args}); err != nil {
			return msgs, err
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
		msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: truncateToolResult(tres.Output)})
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
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = safeCall(ctx, logger, tool, tc.Name, tc.Args)
		}(i, tc, tool)
	}
	wg.Wait()
	return results
}

func truncateToolResult(s string) string {
	return truncate(s, maxToolResultInHistory, "…[tool_result 已截断]")
}
