package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/justphantom/miniagent/text"
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
	// The burn evidence is Reasoning OR ReasoningState: some endpoints return only the opaque wire state
	// (no reasoning_content text), and that alone still proves the budget went to thinking.
	if err == nil && resp.FinishReason == "length" && resp.Text == "" && (resp.Reasoning != "" || resp.ReasoningState != "") && !downgraded && cfg.ThinkingLevel != "" && cfg.ThinkingLevel != ThinkingOff {
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

// applyBeforeLLM invokes hooks.BeforeLLM (nil=pass-through, minimal mode) and folds its side effects on
// transcript / persistence / usage / compaction flags back into the loop's local state. Returns the message view (toSend) actually sent to the LLM this turn.
func applyBeforeLLM(ctx context.Context, hooks LoopHooks, step int, msgs, newMsgs *[]Message, total *Usage, compacted *bool, cfg LoopConfig) ([]Message, int, error) {
	if hooks.BeforeLLM == nil {
		return *msgs, 0, nil
	}
	out, err := hooks.BeforeLLM(ctx, StepInput{Step: step, Msgs: *msgs, System: cfg.System, Tools: cfg.Tools})
	if err != nil {
		return nil, 0, err
	}
	toSend := out.View
	if len(toSend) == 0 {
		// Both nil and empty slice fall back to *msgs: under Commit=true an empty View (including []Message{}) would silently clear the transcript;
		// the core does not require View to be non-empty, so guard here (a legitimate compaction keeps at least the recent turn + user prompt; an empty View is always misuse).
		toSend = *msgs
	}
	if out.Commit {
		// Compaction scenario: the shrunk View is the new running transcript (len guard already applied, will not clear).
		*msgs = toSend
	}
	if len(out.Persist) > 0 {
		mergePersisted(newMsgs, out.Persist)
	}
	if out.ExtraUsage != nil {
		total.InputTokens += out.ExtraUsage.InputTokens
		total.OutputTokens += out.ExtraUsage.OutputTokens
	}
	if out.Compacted {
		*compacted = true
	}
	// Thread the compaction pass's request-side estimate (out.ViewEstimate, 0 on non-compaction) to OnBudget, so the zero-usage
	// streaming path can reuse it instead of a duplicate EstimateTokens scan of toSend (st2).
	return toSend, out.ViewEstimate, nil
}

// mergePersisted bulk-merges persisted into newMsgs: entries with a non-empty Kind replace the old entry of the same Kind in newMsgs
// (e.g. multiple compactions keep only the latest summary), then prepends the batch to the front of newMsgs — preserving the semantics
// of the cross-turn barrier hitting the latest summary (generalized to any persisted entry carrying a Kind).
func mergePersisted(newMsgs *[]Message, persisted []Message) {
	kinds := make(map[string]bool, len(persisted))
	for _, m := range persisted {
		if m.Kind != "" {
			kinds[m.Kind] = true
		}
	}
	filtered := make([]Message, 0, len(*newMsgs)+len(persisted))
	for _, m := range *newMsgs {
		if m.Kind != "" && kinds[m.Kind] {
			continue
		}
		filtered = append(filtered, m)
	}
	merged := make([]Message, 0, len(filtered)+len(persisted))
	merged = append(merged, persisted...)
	merged = append(merged, filtered...)
	*newMsgs = merged
}

// appendMsg appends to both msgs (LLM context) and newMsgs (persisted). All producers go through this uniformly,
// keeping session persistence consistent with the context. Messages with Ts==0 get a Unix-millisecond timestamp (for external strategies'
// "real usage anti-staleness" check); an explicitly set Ts (e.g. a compaction summaryMsg) is not overwritten — 0 reliably means "not carried".
func appendMsg(msgs, newMsgs *[]Message, m Message) {
	if m.Ts == 0 {
		m.Ts = text.NowMs()
	}
	*msgs = append(*msgs, m)
	*newMsgs = append(*newMsgs, m)
}

// recordStepUsage accumulates a single step's real usage into total and runs the zero-usage estimation fallback + budget check via the OnBudget hook.
// Shared by the main path and the summary path, eliminating the duplicated three-part logic in two places (AfterLLM stays at each caller because its Steps
// semantics and Result construction are coupled). toSend is the message view actually sent to the LLM this turn, used by OnBudget for estimation.
func recordStepUsage(ctx context.Context, hooks LoopHooks, step int, resp Response, toSend []Message, viewEstimate int, cfg LoopConfig, total *Usage) error {
	total.InputTokens += resp.Usage.InputTokens
	total.OutputTokens += resp.Usage.OutputTokens
	if hooks.OnBudget != nil {
		if berr := hooks.OnBudget(ctx, step, BudgetInput{ToSend: toSend, System: cfg.System, Tools: cfg.Tools, Resp: resp, PreEstimate: viewEstimate}, total); berr != nil {
			return berr
		}
	}
	if hooks.OnStepUsage != nil {
		hooks.OnStepUsage(step, resp.Usage.InputTokens, resp.Usage.OutputTokens, len(resp.ToolCalls))
	}
	return nil
}
