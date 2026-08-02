package miniagent

import (
	"context"
	"errors"
	"fmt"

	"log/slog"
)

// callLLM 保持原签名供 thinking_test.go 直接调用，委托 callLLMWithDowngrade 并丢弃
// downgraded。Run 改用 callLLMWithDowngrade 以跨步固化 thinking 降级（P2-2）。
// stream 可为 nil（非流式模式不会用到）；流式模式下 stream==nil 由调用方保证不进入。
func callLLM(ctx context.Context, chat *ChatClient, stream *StreamClient, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks, logger *slog.Logger) (Response, error) {
	resp, _, err := callLLMWithDowngrade(ctx, chat, stream, cfg, step, msgs, hooks, logger)
	return resp, err
}

// callLLMWithDowngrade：callLLMOnce 之上做单步 thinking 降级重试，回传 downgraded 供 Run
// 跨步固化 cfg；其余（重试一次、日志、截断告警）与原 callLLM 一致。
func callLLMWithDowngrade(ctx context.Context, chat *ChatClient, stream *StreamClient, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks, logger *slog.Logger) (Response, bool, error) {
	if logger != nil {
		logger.Debug("llm call start", "step", step, "model", cfg.Model, "stream", cfg.Stream, "thinking", cfg.ThinkingLevel)
	}
	resp, err := callLLMOnce(ctx, chat, stream, cfg, step, msgs, hooks)
	downgraded := false
	// thinking 不被支持：去字段重试一次（仅当确实发了 thinking，避免无谓重试，审查 v2 #7）。
	if errors.Is(err, ErrThinkingUnsupported) && cfg.ThinkingLevel != "" && cfg.ThinkingLevel != ThinkingOff {
		if logger != nil {
			logger.Warn("thinking 不被端点支持，去字段重试一次", "step", step)
		}
		down := cfg
		down.ThinkingLevel = ""
		down.Thinking = nil
		resp, err = callLLMOnce(ctx, chat, stream, down, step, msgs, hooks)
		downgraded = true
	}
	if err != nil {
		if logger != nil {
			logger.Warn("llm call failed", "step", step, "error", err)
		}
		return Response{}, downgraded, fmt.Errorf("llm call %d: %w", step, err)
	}
	if logger != nil {
		logger.Info("llm call done", "step", step, "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "tool_calls", len(resp.ToolCalls), "finish_reason", resp.FinishReason)
	}
	// finish_reason 非 stop/tool_calls 表示回答被 max_tokens 或内容过滤截断。
	if logger != nil && resp.FinishReason != "" && resp.FinishReason != "stop" && resp.FinishReason != "tool_calls" {
		logger.Warn("llm response truncated", "step", step, "finish_reason", resp.FinishReason)
	}
	return resp, downgraded, nil
}

// callLLMOnce 构造 Request（含 thinking）并单次调用 chat.Do/stream.DoStream，不含降级/重试逻辑。
//
// 放在 loop_extra.go 是为了携带 panic 兜底（审查 P3 LLM panic 兜底）：Do/DoStream 的响应
// 解析路径（parseChatResponse / parseSSE）在畸形 payload 上可能 panic，无兜底则单次坏响应
// 就崩进程——与防 tool panic 的 safeCall 不对称。recover 转为 error，使 callLLMWithDowngrade
// 沿正常 error 路径处理（降级/重试逻辑仍适用，误判无害）。命名返回值让 defer 能在 panic 后赋值 err。
func callLLMOnce(ctx context.Context, chat *ChatClient, stream *StreamClient, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks) (resp Response, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("llm call panicked: %v", r)
		}
	}()
	req := Request{
		Model:         cfg.Model,
		System:        cfg.System,
		Messages:      msgs,
		MaxTokens:     cfg.MaxTokens,
		Tools:         cfg.Tools,
		ThinkingLevel: cfg.ThinkingLevel,
		Thinking:      cfg.Thinking,
	}
	if cfg.Stream {
		return stream.DoStream(ctx, req, func(d Delta) {
			if hooks.OnDelta != nil {
				_ = hooks.OnDelta(step, d.Kind, d.Text)
			}
		})
	}
	return chat.Do(ctx, req)
}
