package policy

import (
	"context"
	"errors"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"log/slog"
	"time"
	"unicode/utf8"
)

// This file carries the default hook implementations of the three policies originally built into Run core,
// for reuse by the cmd layer when assembling LoopHooks — so "core has zero policies" and "out-of-the-box
// default behavior" coexist: the core loop reads no policies and does no estimation/budgeting/persistence/error
// recovery; callers restore the original built-in semantics by wiring these factories in one line.

// NewDefaultOnLLMError returns the default OnLLMError hook that carries the miniagent.ErrContextLength
// tightening retry. Triggered only for miniagent.ErrContextLength: after trimHistoryForContext (clears reasoning
// + compresses tool content) tightens the history, retry=true and the core retries this call once; other errors
// pass through (retry=false) and the core propagates them as usual. logger is only used for the tightening warn.
func NewDefaultOnLLMError(logger *slog.Logger, contextTrim int) func(context.Context, int, []miniagent.Message, error) ([]miniagent.Message, bool, error) {
	return func(_ context.Context, _ int, msgs []miniagent.Message, err error) ([]miniagent.Message, bool, error) {
		if !errors.Is(err, miniagent.ErrContextLength) {
			return nil, false, nil
		}
		if logger != nil {
			logger.Warn("context length exceeded; trimmed history; retrying step")
		}
		return trimHistoryForContext(msgs, contextTrim), true, nil
	}
}

// NewDefaultOnBudget returns the default OnBudget hook that carries the zero-usage local estimation fallback
// + the MaxTotalTokens budget decision. The core has already accumulated the real usage into total; when
// resp.Usage is all-zero (streaming endpoints often do not honor include_usage / non-streaming may omit it),
// this hook supplements a local estimate (EstimateTokens counts the request side toSend+system+tools,
// estimateResponseTokens counts the response side), then judges circuit-break by maxTotalTokens. maxTotalTokens<=0
// skips the check.
func NewDefaultOnBudget(maxTotalTokens int, logger *slog.Logger) func(context.Context, int, miniagent.BudgetInput, *miniagent.Usage) error {
	return func(_ context.Context, step int, in miniagent.BudgetInput, total *miniagent.Usage) error {
		// Zero-usage fallback: estimate the missing tokens per side (R4-7). The original && estimated only when
		// fully zero; a half-zero usage (e.g. only prompt_tokens returned) accumulated the missing side as 0,
		// causing systematic budget underestimation; now each side is estimated independently.
		inZero := in.Resp.Usage.InputTokens == 0
		outZero := in.Resp.Usage.OutputTokens == 0
		if inZero || outZero {
			if logger != nil {
				logger.Warn("llm returned partial/zero usage; estimating missing side(s) locally", "step", step)
			}
			if inZero {
				// Reuse the compaction pass's already-computed request estimate when available (streaming zero-usage path) instead
				// of a duplicate EstimateTokens scan of ToSend (st2). PreEstimate is 0 on the non-compaction path → estimate as before.
				if in.PreEstimate > 0 {
					total.InputTokens += in.PreEstimate
				} else {
					total.InputTokens += EstimateTokens(in.ToSend, in.System, in.Tools)
				}
			}
			if outZero {
				total.OutputTokens += EstimateResponseTokens(in.Resp)
			}
		}
		if maxTotalTokens > 0 && total.InputTokens+total.OutputTokens > maxTotalTokens {
			return miniagent.ErrBudgetExceeded
		}
		return nil
	}
}

// NewDefaultShapeToolResult returns the default ShapeToolResult hook that carries the original defaultShapeResult
// semantics: trimForHistory truncation (miniagent.Tool.ResultLimit/SplitTruncate, falling back
// maxToolResultChars→MaxToolResultInHistory) + optional persistence (when dir is non-empty, writes the full text
// to disk if it exceeds the limit, replacing Content with a preview + path hint). dir empty = persistence disabled
// (truncate only). At construction it creates the store once and cleans up expired files (matching the
// single-run CLI shape). tools is used to read each tool's ResultLimit/SplitTruncate.
func NewDefaultShapeToolResult(tools []miniagent.Tool, dir string, retention time.Duration, maxToolResultChars int, logger *slog.Logger) func(string, string, int, miniagent.ToolResult) (string, error) {
	toolByName := make(map[string]miniagent.Tool, len(tools))
	for _, t := range tools {
		toolByName[t.Name] = t
	}
	var store *toolOutputStore
	if dir != "" {
		store = newToolOutputStore(dir, retention, logger)
		store.cleanup()
	}
	return func(name, callID string, step int, tres miniagent.ToolResult) (string, error) {
		limit := 0
		split := false
		if t, ok := toolByName[name]; ok {
			limit = t.ResultLimit
			split = t.SplitTruncate
		}
		// When the tool does not declare ResultLimit, fall back to maxToolResultChars; trimForHistory still has
		// the final <=0→MaxToolResultInHistory fallback as a double safety net. split enables head+tail segmented
		// truncation for shell/grep-like tools.
		if limit <= 0 {
			limit = maxToolResultChars
		}
		preview := TrimForHistory(tres.Output, limit, split)
		content := preview
		if store != nil {
			// §P1-A: the full text exceeding the limit is persisted to disk, and the history Content is replaced
			// with a preview + path hint. The truncated decision uses an exact comparison against the effective
			// limit (trimForHistory internally falls back to MaxToolResultInHistory when limit<=0; the same
			// resolution is replicated here; truncation occurs iff the rune count > effective limit), avoiding
			// false negatives of the rune-length-diff approach when "the output just exceeds the limit and the
			// preview marker is longer anyway" (the full text would be lost and not persisted).
			effLimit := limit
			if effLimit <= 0 {
				effLimit = miniagent.MaxToolResultInHistory
			}
			content = store.bound(step, callID, tres.Output, preview, utf8.RuneCountInString(tres.Output) > effLimit)
		}
		return content, nil
	}
}
