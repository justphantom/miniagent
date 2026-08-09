package miniagent

import (
	"context"
	"errors"
	"fmt"

	"log/slog"
)

// callLLMWithDowngrade performs a single-step thinking downgrade retry on top of callLLMOnce, returning
// downgraded so Run can persist cfg across steps; everything else (one-shot retry, logging, truncation
// warning) is kept as-is.
func callLLMWithDowngrade(ctx context.Context, llm LLM, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks, logger *slog.Logger) (resp Response, downgraded bool, requests int, err error) {
	if logger != nil {
		logger.Debug("llm call start", "step", step, "model", cfg.Model, "stream", cfg.Stream, "thinking", cfg.ThinkingLevel)
	}
	resp, err = callLLMOnce(ctx, llm, cfg, step, msgs, hooks)
	requests = 1
	// thinking unsupported: retry once with the field dropped (only when thinking was actually sent, to avoid a pointless retry; review v2 #7).
	if errors.Is(err, ErrThinkingUnsupported) && cfg.ThinkingLevel != "" && cfg.ThinkingLevel != ThinkingOff {
		if logger != nil {
			logger.Warn("thinking unsupported by endpoint, retrying once with the field dropped", "step", step)
		}
		down := cfg
		down.ThinkingLevel = ""
		down.Thinking = nil
		resp, err = callLLMOnce(ctx, llm, down, step, msgs, hooks)
		downgraded = true
		requests = 2
	}
	// R1 defense: finish_reason=length with empty text means the model burned its output budget (typically on
	// reasoning_content) and returned no usable content — treating that as the final reply silently truncates the
	// turn (kimi/k3 and other long-reasoning models). When thinking was actually sent, retry once with thinking
	// dropped to free the output budget for real content (mirrors the thinking-downgrade retry above; the single
	// retry + downgraded flag prevents any loop). Narrow by design — only the empty-text sub-case: mid-sentence-cut
	// text is left to the truncation warn below, since cheap auto-continuation isn't feasible. Best-effort: some
	// providers don't free output budget when thinking drops, so the rescue is not guaranteed.
	if err == nil && resp.FinishReason == "length" && resp.Text == "" && resp.Reasoning != "" && !downgraded && cfg.ThinkingLevel != "" && cfg.ThinkingLevel != ThinkingOff {
		if logger != nil {
			logger.Warn("length truncation with empty text; retrying once with thinking dropped to free output budget", "step", step)
		}
		down := cfg
		down.ThinkingLevel = ""
		down.Thinking = nil
		resp, err = callLLMOnce(ctx, llm, down, step, msgs, hooks)
		downgraded = true
		requests++
	}
	if err != nil {
		if logger != nil {
			logger.Warn("llm call failed", "step", step, "error", err)
		}
		return Response{}, downgraded, requests, fmt.Errorf("llm call %d: %w", step, err)
	}
	if logger != nil {
		logger.Info("llm call done", "step", step, "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "tool_calls", len(resp.ToolCalls), "finish_reason", resp.FinishReason)
	}
	// A finish_reason other than stop/tool_calls means the answer was truncated by max_tokens or content
	// filtering. The core only warns and does not continue — the truncated resp.Text is still processed as
	// the final text / tool calls (an LLM-layer limitation; the core cannot cheaply auto-continue).
	if logger != nil && resp.FinishReason != "" && resp.FinishReason != "stop" && resp.FinishReason != "tool_calls" {
		logger.Warn("llm response truncated", "step", step, "finish_reason", resp.FinishReason)
	}
	return resp, downgraded, requests, nil
}

// callLLMOnce builds the Request (including thinking) and performs a single llm.Do / llm.DoStream call,
// with no downgrade/retry logic.
//
// It lives in loop_extra.go to carry the panic fallback (review P3 LLM panic fallback): the response parse
// paths of Do/DoStream (parseChatResponse / parseSSE) may panic on malformed payloads, and without a
// fallback a single bad response would crash the process — asymmetric with safeCall which guards tool
// panics. recover converts the panic into an error so callLLMWithDowngrade handles it along the normal
// error path (the downgrade/retry logic still applies, and a misjudgment is harmless). Named return values
// let the defer assign err after a panic.
func callLLMOnce(ctx context.Context, llm LLM, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks) (resp Response, err error) {
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
		return llm.DoStream(ctx, req, func(d Delta) error {
			if hooks.OnDelta != nil {
				return hooks.OnDelta(step, d.Kind, d.Text)
			}
			return nil
		})
	}
	return llm.Do(ctx, req)
}
