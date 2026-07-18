package miniagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"log/slog"
)

const maxIterations = 20
const maxToolResultInHistory = 2000 // 每条 tool 结果进入历史的字符上限

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
	toolSpecs := make([]ToolSpec, 0, len(cfg.Tools))
	toolByName := make(map[string]Tool, len(cfg.Tools))
	for _, t := range cfg.Tools {
		toolSpecs = append(toolSpecs, ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		toolByName[t.Name] = t
	}
	emitSignal := func(sig Signal) error {
		if emit != nil {
			return emit(sig)
		}
		return nil
	}

	userMsg := Message{Role: "user", Content: userPrompt}
	msgs := make([]Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, userMsg)

	var total Usage
	for step := 1; step <= maxIterations; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1}, err
		}
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
			return Result{Usage: total, Steps: step - 1}, fmt.Errorf("llm call %d: %w", step, err)
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		if logger != nil {
			logger.Info("llm call done", "prompt_id", promptID, "step", step, "duration_ms", callDur.Milliseconds(), "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "tool_calls", len(resp.ToolCalls))
		}

		if len(resp.ToolCalls) == 0 {
			msgs = append(msgs, Message{Role: "assistant", Content: resp.Text})
			return Result{Text: resp.Text, Usage: total, Steps: step, NewMessages: msgs[len(history):]}, nil
		}

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
				return Result{Usage: total, Steps: step}, err
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
				return Result{Usage: total, Steps: step}, err
			}
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: truncateToolResult(tres.Output)})
		}
	}
	// 达到迭代上限时返回 nil error + Incomplete=true，让上层仍能消费
	// 已累积的 Usage/History，避免烧掉的 token 全部丢弃。
	return Result{Usage: total, Steps: maxIterations, NewMessages: msgs[len(history):], Incomplete: true}, nil
}

func truncateToolResult(s string) string {
	return truncate(s, maxToolResultInHistory, "…[tool_result 已截断]")
}
