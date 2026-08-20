// budget.go: budget estimation, configuration constants, and the FitHistory entry point.

package compaction

import (
	"context"
	"fmt"
	"github.com/justphantom/miniagent/miniagent/policy"
	"log/slog"

	"github.com/justphantom/miniagent/miniagent"
)

const (
	// summaryMaxChars is the built-in upper bound on the number of characters in a summary (initial value, tuned
	// empirically, design §15.4). By default deriveSummaryMaxChars scales it with the window via
	// min(summaryMaxChars, ContextWindow/summaryCharsPerWindowRatio) (direction A) — large windows take this value,
	// small windows adapt, avoiding summary itself > CW×4/5 causing termination after compaction (boundary of B).
	summaryMaxChars = 5000
	// summaryCharsPerWindowRatio: default summaryMaxChars = ContextWindow/this. Using 5 → summary tokens
	// (chars/2) occupy ~10% of CW, leaving ~90% for head+tail+LLM output.
	summaryCharsPerWindowRatio = 5
	// summaryMaxTokens is the fallback constant for the summary output token cap, derived from summaryMaxChars (/2).
	summaryMaxTokens = summaryMaxChars / 2
)

// ContextBudget is the single parameter bundle for FitHistory: context window, rounds to keep, summary prompt and model,
// and an injectable callback to compress the middle into a summary (decouples context.go from HTTPClient, allows test injection of fake summaries).
// System/Tools are used by policy.EstimateTokens to account for the fixed overhead of system prompt and tool schema.
type ContextBudget struct {
	ContextWindow int // 0 = no proactive compaction
	KeepRecent    int // <=0 falls back to contextKeepRecent
	KeepReasoning int
	KeepToolArgs  int
	// KeepReasoningChars is the character cap for a single Reasoning message within the keep window (P7).
	// 0=falls back to contextKeepReasoningChars; <0=disabled (no truncation); >0=custom threshold.
	KeepReasoningChars int
	SummarizerPrompt   string
	CompactionModel    string // empty falls back to Model
	Model              string
	System             string
	Tools              []miniagent.Tool
	UseRealUsage       bool
	// Force=true causes FitHistory to skip the policy.EstimateTokens 4/5 gate and go directly into compactWithSummary+lossy fallback.
	Force bool
	// PreserveRecentTokens is the token budget upper bound for retainedTail (§P1-E).
	PreserveRecentTokens int
	// SummaryMaxChars is the character cap for the summary, used by jointTailBudget.
	SummaryMaxChars int
	// Compacting triggers before each summary (inside compactWithSummary, before calling budget.Summarize).
	// nil=disabled.
	Compacting CompactingHook
	// SessionID is carried through budget into the compactWithSummary scope.
	SessionID string
	// Summarize compresses the middle segment into summary text + the miniagent.Usage of that call.
	Summarize func(ctx context.Context, model, summarizerPrompt, previousSummary string, middle []miniagent.Message) (string, miniagent.Usage, error)
}

// FitHistory is the single entry point of Run phase 2: summarize middle when over 80% of window, fall back to
// lossy compaction on failure/no middle, then trim to most recent round if still over, then error if still over
// (review v2 #8 + v3 #4). Returns:
//   - out: processed msgs (may contain new summary);
//   - summary: the miniagent.KindSummary message generated this round (summary.Kind=="" means none generated, caller uses this to detect summarized);
//   - summarized: whether summary compaction succeeded (Run uses this to set result.Compacted and insert summary into newMsgs);
//   - committed: whether this round's out should replace the running transcript (compaction success/fallback=true; non-compaction strip only this round's View=false).
//     Non-compaction does not replace → transcript keeps original (reasoning/args not lost to rolling strip); compaction replaces with [head,summary,tail],
//     tail keeps original (no longer stripped out), RewriteMessages persists complete recent context (eliminating compaction-round persistence asymmetry).
//   - usage: token usage of the summary call (Run accumulates into total, MaxTotalTokens budget includes summary call);
//   - err: returned when still over window even after lossy trim; Run should stop to avoid burning requests in a loop.
//
// Does not touch newMsgs — persistence-layer summary insertion/dedup is done by Run via mergePersisted (loop.go:216).
func FitHistory(ctx context.Context, msgs []miniagent.Message, budget ContextBudget, logger *slog.Logger) (out []miniagent.Message, summary miniagent.Message, summarized, committed bool, usage miniagent.Usage, viewEstimate int, err error) {
	keepReasoning := budget.KeepReasoning
	if keepReasoning <= 0 {
		keepReasoning = contextKeepReasoning
	}
	keepToolArgs := budget.KeepToolArgs
	if keepToolArgs <= 0 {
		keepToolArgs = contextKeepToolArgs
	}
	keepReasoningChars := budget.KeepReasoningChars
	if keepReasoningChars == 0 {
		keepReasoningChars = contextKeepReasoningChars
	}
	// keepReasoningChars < 0 → truncateKeptReasoning internally threshold<=0 returns as-is (disabled).
	// Gating based on strip View (=what LLM sees), not original msgs: non-compaction step Commit=false then transcript keeps original, each round recomputes strip from original.
	// §P0-B: estimateThreshold prefers real usage, falls back to local estimate, covering the blind spot of policy.EstimateTokens for cached content.
	// §P1-B: Force=true (last step's real usage hit isUsageOverflow) skips gating, directly enters compaction branch (Force only set when CW>0).
	if !budget.Force {
		stripped := applyContextStrips(ctx, msgs, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
		if budget.ContextWindow <= 0 || estimateThreshold(stripped, budget.System, budget.Tools, budget.UseRealUsage) <= budget.ContextWindow*4/5 {
			// Non-compaction: strip only this round's View (committed=false), transcript keeps original, next round recomputes strip from original.
			return stripped, miniagent.Message{}, false, false, miniagent.Usage{}, 0, nil
		}
	}
	keepRecent := budget.KeepRecent
	if keepRecent <= 0 {
		keepRecent = contextKeepRecent
	}
	fitted, sm, sumUsage, serr := compactWithSummary(ctx, budget, msgs, keepRecent)
	if serr != nil {
		if logger != nil {
			logger.Warn("summarize failed, falling back to lossy compaction", "error", serr)
		}
	} else if sm.Kind != miniagent.KindSummary && logger != nil {
		logger.Warn("no middle to summarize, lossy compaction")
	}
	summarized = sm.Kind == miniagent.KindSummary
	out = fitted
	if !summarized {
		// fallback lossy trim (raw): no jointTailBudget fallback, keep strip to prevent overflow.
		out = compactHistory(msgs, keepRecent)
		out = applyContextStrips(ctx, out, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
	} else {
		// §P1-post: apply the steady-state reasoning strip to the TAIL subslice only (head + summaryMsg untouched).
		// out = [head?(nil), summaryMsg, tail...] and the new summaryMsg is the boundary compactWithSummary created;
		// summaryTailStart locates it (the single miniagent.KindSummary in out) and everything after it is the tail.
		// Without this, the one post-compaction request — the most context-starved request of the run — replays the
		// retained tail's full reasoning verbatim; next turn's P1 strip would clear it anyway, so applying the SAME
		// keepReasoning / keepReasoningChars here removes that one oversized request with zero steady-state divergence.
		// Only the reasoning strips (P1 stripStaleReasoning + P7 truncateKeptReasoning) are applied — mirroring "next
		// turn P1" and keeping the change minimal; head + summaryMsg precede the boundary and are left untouched.
		if tailStart := summaryTailStart(out); tailStart >= 0 && tailStart < len(out) {
			strippedTail := stripStaleReasoning(out[tailStart:], keepReasoning)
			strippedTail = truncateKeptReasoning(strippedTail, keepReasoning, keepReasoningChars)
			combined := make([]miniagent.Message, 0, tailStart+len(strippedTail))
			combined = append(combined, out[:tailStart]...)
			combined = append(combined, strippedTail...)
			out = combined
		}
	}
	// summarized: out=fitted with only the tail reasoning stripped (head + summaryMsg untouched); overall size still
	// controlled by jointTailBudget (≤ CW×4/5).
	// Post-compaction uses local EstimateTokens for gating (estimateThreshold reflects pre-compaction prefix; post-compaction real usage is stale, local is more accurate).
	if policy.EstimateTokens(out, budget.System, budget.Tools) > budget.ContextWindow*4/5 {
		out = trimRecentRounds(out, keepRecent)
		if logger != nil {
			logger.Warn("still exceeds window, trimming to recent rounds", "msgs", len(out))
		}
	}
	// Cache post-trim out token estimate: reused by both the check below and the error message (eliminates one of the original three EstimateTokens calls).
	est := policy.EstimateTokens(out, budget.System, budget.Tools)
	if est > budget.ContextWindow*4/5 {
		return out, sm, summarized, true, sumUsage, est, fmt.Errorf("history exceeds context window (~%d tokens) even after lossy trimming — terminating to avoid burning requests in a loop", est)
	}
	return out, sm, summarized, true, sumUsage, est, nil
}

// applyContextStrips runs all proactive trims (P1/P4/P6/P7/P8'/P9b/P11), only modifying the context-side copy,
// reused by both FitHistory's not-over-window and over-window branches (the original two inline sequences were
// identical; extracted here to avoid duplication and unify observability). logger at Debug level (CLI -log-level debug)
// logs tokens saved at each stage (policy.EstimateTokens delta) and total before/after fit, for v11 §6 runtime
// confirmation of "actually saved"; Info level (default) computes no delta, zero overhead. See each strip function's doc for semantics.
func applyContextStrips(ctx context.Context, msgs []miniagent.Message, keepReasoning, keepReasoningChars, keepToolArgs int, logger *slog.Logger, sys string, tools []miniagent.Tool) []miniagent.Message {
	dbg := logger != nil && logger.Enabled(ctx, slog.LevelDebug)
	strip := func(stage string, fn func([]miniagent.Message) []miniagent.Message, in []miniagent.Message) []miniagent.Message {
		if !dbg {
			return fn(in)
		}
		before := policy.EstimateTokens(in, sys, tools)
		o := fn(in)
		if after := policy.EstimateTokens(o, sys, tools); before > after {
			logger.Debug("context budget: strip saved",
				"stage", stage, "saved_tokens", before-after, "before_msgs", len(in), "after_msgs", len(o))
		}
		return o
	}
	out := strip("P1_reasoning", func(m []miniagent.Message) []miniagent.Message { return stripStaleReasoning(m, keepReasoning) }, msgs)
	out = strip("P7_reasoningTrunc", func(m []miniagent.Message) []miniagent.Message {
		return truncateKeptReasoning(m, keepReasoning, keepReasoningChars)
	}, out)
	out = strip("P4_toolArgs", func(m []miniagent.Message) []miniagent.Message { return stripStaleToolArgs(m, keepToolArgs) }, out)
	out = strip("P6_dedupRead", func(m []miniagent.Message) []miniagent.Message { return dedupReadResults(m, keepToolArgs) }, out)
	out = strip("P11_foldRead", func(m []miniagent.Message) []miniagent.Message { return foldStaleReadResults(m, keepToolArgs) }, out)
	out = strip("P8p_foldWriteEdit", func(m []miniagent.Message) []miniagent.Message { return foldStaleWriteEditArgs(m, keepToolArgs) }, out)
	out = strip("P9b_dedupShell", func(m []miniagent.Message) []miniagent.Message { return dedupShellCommands(m, keepToolArgs) }, out)
	if dbg {
		logger.Debug("context budget: fit done",
			"before_tokens", policy.EstimateTokens(msgs, sys, tools), "after_tokens", policy.EstimateTokens(out, sys, tools), "msgs", len(out))
	}
	return out
}

// summaryTailStart / preserveRecentTokens / jointTailBudget / estimateRoundTokens live in budget_tail.go.
