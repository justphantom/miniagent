package miniagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"log/slog"
)

// 单轮对话的 LLM 调用上限：防止模型陷入工具循环烧 token，按典型工具链
// （读→改→测→答）所需步骤数 + 适度余量设定。
const maxIterations = 20

// 单条 tool 结果进入历史的字符上限：超长输出会稀释上下文预算，2k 字符
// 既能保留工具结果的可读信息，又不至于挤占 LLM 的输入窗口。
const maxToolResultInHistory = 2000

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

// Run drives the ReAct loop for one turn.
func Run(ctx context.Context, llm *HTTPClient, cfg LoopConfig, promptID, userPrompt string, history []Message, emit EmitFunc, logger *slog.Logger) (Result, error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm client is nil")
	}
	toolSpecs, toolByName := buildToolIndex(cfg.Tools)
	emitSignal := func(sig Signal) error {
		if emit != nil {
			return emit(sig)
		}
		return nil
	}

	msgs := makeUserMessages(history, userPrompt)
	total := Usage{}

	for step := 1; step <= maxIterations; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1}, err
		}
		resp, err := callLLM(ctx, llm, cfg, promptID, step, msgs, toolSpecs, logger)
		if err != nil {
			return Result{Usage: total, Steps: step - 1}, err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens

		if len(resp.ToolCalls) == 0 {
			msgs = append(msgs, Message{Role: "assistant", Content: resp.Text})
			return Result{Text: resp.Text, Usage: total, Steps: step, NewMessages: msgs[len(history):]}, nil
		}

		msgs, err = handleToolCalls(ctx, promptID, step, resp, toolByName, msgs, emitSignal, logger)
		if err != nil {
			return Result{Usage: total, Steps: step}, err
		}
	}
	// 达到迭代上限时返回 nil error + Incomplete=true，让上层仍能消费
	// 已累积的 Usage/History，避免烧掉的 token 全部丢弃。
	return Result{Usage: total, Steps: maxIterations, NewMessages: msgs[len(history):], Incomplete: true}, nil
}

func buildToolIndex(tools []Tool) ([]ToolSpec, map[string]Tool) {
	toolSpecs := make([]ToolSpec, 0, len(tools))
	toolByName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		toolSpecs = append(toolSpecs, ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		toolByName[t.Name] = t
	}
	return toolSpecs, toolByName
}

func makeUserMessages(history []Message, userPrompt string) []Message {
	userMsg := Message{Role: "user", Content: userPrompt}
	msgs := make([]Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, userMsg)
	return msgs
}

func callLLM(ctx context.Context, llm *HTTPClient, cfg LoopConfig, promptID string, step int, msgs []Message, toolSpecs []ToolSpec, logger *slog.Logger) (Response, error) {
	msgs = trimMessages(msgs, maxHistoryTokens)
	if logger != nil {
		logger.Debug("llm call start", "prompt_id", promptID, "step", step, "model", cfg.Model)
	}
	callStart := time.Now()
	system := cfg.System
	if cfg.MemoryContext != "" {
		system += cfg.MemoryContext
	}
	resp, err := llm.Do(ctx, Request{
		Model:     cfg.Model,
		System:    system,
		Messages:  msgs,
		MaxTokens: cfg.MaxTokens,
		Tools:     toolSpecs,
	})
	callDur := time.Since(callStart)
	if err != nil {
		if logger != nil {
			logger.Warn("llm call failed", "prompt_id", promptID, "step", step, "error", err, "duration_ms", callDur.Milliseconds())
		}
		return Response{}, fmt.Errorf("llm call %d: %w", step, err)
	}
	if logger != nil {
		logger.Info("llm call done", "prompt_id", promptID, "step", step, "duration_ms", callDur.Milliseconds(), "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "tool_calls", len(resp.ToolCalls), "finish_reason", resp.FinishReason)
	}
	// finish_reason 非 stop/tool_calls 表示回答被 max_tokens 或内容过滤截断，
	// 最终文本是不完整的，调用方应感知。这里仅告警不改返回结构，避免破坏现有契约。
	if logger != nil && resp.FinishReason != "" && resp.FinishReason != "stop" && resp.FinishReason != "tool_calls" {
		logger.Warn("llm response truncated", "prompt_id", promptID, "step", step, "finish_reason", resp.FinishReason)
	}
	return resp, nil
}

func handleToolCalls(ctx context.Context, promptID string, step int, resp Response, toolByName map[string]Tool, msgs []Message, emitSignal func(Signal) error, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	msgs = append(msgs, Message{Role: "assistant", ToolCalls: calls})

	for _, tc := range calls {
		if err := emitSignal(Signal{Kind: SignalToolUse, Name: tc.Name, Input: tc.Args}); err != nil {
			return msgs, err
		}
		tool, ok := toolByName[tc.Name]
		var tres ToolResult
		if !ok {
			tres = ToolResult{IsError: true, Output: fmt.Sprintf("未知工具 %q", tc.Name)}
		} else {
			tres = safeCall(ctx, logger, tool, tc.Name, tc.Args)
		}
		if logger != nil {
			logger.Info("tool executed", "prompt_id", promptID, "step", step, "tool", tc.Name, "is_error", tres.IsError, "output_len", len(tres.Output))
		}
		if err := emitSignal(Signal{Kind: SignalToolResult, Name: tc.Name, Input: tc.Args, Output: tres.Output, IsError: tres.IsError}); err != nil {
			return msgs, err
		}
		msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: truncateToolResult(tres.Output)})
	}
	return msgs, nil
}

func truncateToolResult(s string) string {
	return truncate(s, maxToolResultInHistory, "…[tool_result 已截断]")
}
