// budget.go: budget estimation, configuration constants, and the FitHistory entry point.

package compaction

import (
	"context"
	"fmt"
	"github.com/justphantom/miniagent/internal/miniagent/policy"
	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
)

const (
	// summaryMaxChars is the built-in upper bound on the number of characters in a summary (initial value, tuned
	// empirically, design §15.4). By default deriveSummaryMaxChars scales it with the window via
	// min(summaryMaxChars, ContextWindow/summaryCharsPerWindowRatio) (direction A) — large windows take this value,
	// small windows adapt, avoiding summary itself > CW×4/5 causing termination after compaction (boundary of B).
	// Explicit user summary_max_chars overrides.
	summaryMaxChars = 5000
	// summaryCharsPerWindowRatio: default summaryMaxChars = ContextWindow/this. Using 5 → summary tokens
	// (chars/2) occupy ~10% of CW, leaving ~90% for head+tail+LLM output — balancing "summary information density"
	// against "not squeezing tail/output". Smaller values (e.g. 8) make the summary more compact and lower the CW
	// floor but reduce information; larger values (e.g. 4) do the opposite. Mirrors the built-in ratio style of
	// tailBudgetFraction; explicit user summary_max_chars overrides the derived value. Note: even if summary is
	// shrunk to 0, CW<~1.5k may still terminate (request-level overhead 400 + system + schema + head occupying more
	// than half of CW — a physical limit, not solvable by this ratio; see the hard boundary in deriveSummaryMaxChars).
	summaryCharsPerWindowRatio = 5
	// summaryMaxTokens is the fallback constant for the summary output token cap, derived from summaryMaxChars (/2).
	// Consistent with EstimateTokens' CJK≈1token/2chars: chars/2 is exactly the token upper bound needed for "pure CJK
	// summary filling the chars limit" — pure Chinese exactly fills it (boundary), any sparser content (English
	// paths/symbols) is truncated by chars first, with no extra token truncation or waste. The original fixed 1024 was
	// too tight for CJK: 1024 tokens≈1500 Chinese characters, well below summaryMaxChars=5000, so Chinese summaries
	// were implicitly truncated by MaxTokens to ~30% of the design value (third-evaluation L3-5).
	// Serves as both the fallback for summarizeMiddle (maxSummaryTokens<=0) and the fallback in deriveSummaryMaxTokens
	// for abnormal input.
	summaryMaxTokens = summaryMaxChars / 2
)

// ContextBudget is the single parameter bundle for FitHistory: context window, rounds to keep, summary prompt and model,
// and an injectable callback to compress the middle into a summary (decouples context.go from HTTPClient, allows test injection of fake summaries).
// System/Tools are used by policy.EstimateTokens to account for the fixed overhead of system prompt and tool schema.
type ContextBudget struct {
	ContextWindow int // 0 = no proactive compaction
	KeepRecent    int // <=0 falls back to contextKeepRecent
	// KeepReasoning is the number of most recent assistant messages whose reasoning is preserved during proactive reasoning cleanup (<=0 falls back to contextKeepReasoning).
	// FitHistory clears Reasoning from non-recent N assistant messages on the fitted result (P1).
	KeepReasoning int
	// KeepToolArgs is the number of most recent assistant messages whose args are preserved during proactive tool_call args compression (<=0 falls back to contextKeepToolArgs).
	// FitHistory compresses large write/edit args from non-recent N assistant messages on the fitted result (P4).
	KeepToolArgs int
	// KeepReasoningChars is the character cap for a single Reasoning message within the keep window (P7). After stripStaleReasoning,
	// FitHistory applies head-tail splitting (threshold rune) to overly long Reasoning of the most recent N assistant messages.
	// 0=falls back to contextKeepReasoningChars; <0=disabled (no truncation); >0=custom threshold.
	KeepReasoningChars int
	SummarizerPrompt   string
	CompactionModel    string // empty falls back to Model
	Model              string
	System             string
	Tools              []miniagent.Tool
	// UseRealUsage controls whether estimateThreshold prefers real usage (§P0-B). false (kill-switch)
	// uses local policy.EstimateTokens directly, falling back to old behavior; when true, if no real usage is available it still falls back to local estimation.
	UseRealUsage bool
	// Force=true causes FitHistory to skip the policy.EstimateTokens 4/5 gate and go directly into compactWithSummary+lossy fallback
	// branch (§P1-B). Set by Run when the previous step's real usage hits isUsageOverflow, triggering compaction based on "proven real occupancy".
	Force bool
	// PreserveRecentTokens is the token budget upper bound for retainedTail (§P1-E, ported from opencode preserve_recent_tokens).
	// <=0=auto: floor(ContextWindow/tailBudgetFraction) clamp [min,max]; when ContextWindow<=0 returns 0 to disable,
	// falling back to KeepRecent rounds mode (backward compatible with old sessions and no-window configs).
	PreserveRecentTokens int
	// SummaryMaxChars is the character cap for the summary, used by jointTailBudget to estimate summary token usage (CJK /2 basis). <=0 falls back
	// to the summaryMaxChars constant. Injected by NewCompaction from opts.SummaryMaxChars (already resolved).
	SummaryMaxChars int
	// Compacting triggers before each summary (inside compactWithSummary, before calling budget.Summarize), allowing injection
	// of context or one-time replacement of summarizerPrompt (mirrors opencode experimental.session.compacting, §P2).
	// nil=disabled. Run bridges from LoopHooks.OnCompacting when constructing budget.
	Compacting CompactingHook
	// SessionID is carried through budget into the compactWithSummary scope, read by applyCompactingHook (CompactingInput.SessionID).
	SessionID string
	// Summarize compresses the middle segment into summary text (already truncated to maxChars) + the miniagent.Usage of that call.
	// When previousSummary is non-empty it runs in UPDATE mode (preserving the old summary as an anchor to update), extracted by compactWithSummary
	// from legacy miniagent.KindSummary and passed down via this parameter. Injected by Run: func(ctx, model, summarizerPrompt,
	// previousSummary, middle) → summarizeMiddle(...).
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

// summaryTailStart returns the index in msgs immediately AFTER the first miniagent.KindSummary message (the new summary
// compactWithSummary emits), i.e. the start of the retained tail. Returns -1 when no KindSummary is present. Used by
// FitHistory's summarized branch to apply the steady-state reasoning strip to the tail subslice only — everything
// before the boundary (head + summaryMsg) is left untouched. compactWithSummary emits exactly one KindSummary (the old
// summary, if any, is extracted as previousSummary and never enters out), so the first match is the new summary.
func summaryTailStart(msgs []miniagent.Message) int {
	for i, m := range msgs {
		if m.Kind == miniagent.KindSummary {
			return i + 1
		}
	}
	return -1
}

// preserveRecentTokens resolves the upper bound of the retainedTail token budget (ported from opencode preserveRecentBudget, §P1-E):
// budget.PreserveRecentTokens>0 returned directly; otherwise floor(budget.ContextWindow/tailBudgetFraction)
// clamped to [minPreserveRecentTokens, maxPreserveRecentTokens]; budget.ContextWindow<=0 returns 0 (disabled → pure round-count mode).
// Side-effect free, pure function, easy to test.
func preserveRecentTokens(budget ContextBudget) int {
	if budget.PreserveRecentTokens > 0 {
		return budget.PreserveRecentTokens
	}
	if budget.ContextWindow <= 0 {
		return 0
	}
	t := budget.ContextWindow / tailBudgetFraction
	if t < minPreserveRecentTokens {
		return minPreserveRecentTokens
	}
	if t > maxPreserveRecentTokens {
		return maxPreserveRecentTokens
	}
	return t
}

// jointTailBudget returns the joint token budget for retainedTail (§B): deducts incompressible portions from CW×4/5 —
// request-level system/schema/overhead (EstimateTokens([],System,Tools), appears once per request) + head round marginal +
// summary upper bound estimate (summaryMaxChars/2, CJK densest calibre, same source as summaryMaxTokens derivation +
// single-message envelope) — so tail proactively yields space to the incompressible summary, reducing from the source
// the overflow-induced trim/termination under medium CW where head+summary+tail exceeds window. Takes min with
// preserveRecentTokens (user's tail will upper bound): physical constraint ∧ will upper bound. CW<=0 falls back to
// preserveRecentTokens (windowless pure round-count compatibility).
//
// headAdj refinement: under the default path (SummarizerPrompt==""), when head is an old KindSummary, it's extracted
// as prevSummary for UPDATE and doesn't enter out, so headAdj=0 with no erroneous deduction; otherwise (first round is
// not summary, override path) head enters out → deduct estimateRoundTokens(head). avail<=0 (small CW:
// summary+head+overhead already fills 4/5) → return 0, selectTailByTokens degrades to forcibly retaining most recent round,
// FitHistory's trailing trim/error fallback. This function does not eliminate termination under extremely small CW;
// direction A (deriveSummaryMaxChars scales summaryMaxChars with CW) pushes that boundary up to CW<~1536 — beyond that
// request-level overhead+head dominates, not solvable by compaction budget (physical limit, see deriveSummaryMaxChars hard boundary).
func jointTailBudget(budget ContextBudget, headRounds []miniagent.Message) int {
	if budget.ContextWindow <= 0 {
		return preserveRecentTokens(budget)
	}
	target := budget.ContextWindow * 4 / 5
	reqOverhead := policy.EstimateTokens(nil, budget.System, budget.Tools)
	headAdj := 0
	if len(headRounds) != 1 || headRounds[0].Kind != miniagent.KindSummary || budget.SummarizerPrompt != "" {
		headAdj = estimateRoundTokens(headRounds)
	}
	maxChars := budget.SummaryMaxChars
	if maxChars <= 0 {
		maxChars = summaryMaxChars
	}
	summaryEstimate := maxChars/2 + policy.EnvelopePerMsgTokens
	avail := target - reqOverhead - headAdj - summaryEstimate
	return min(max(avail, 0), preserveRecentTokens(budget))
}

// estimateRoundTokens estimates the marginal tokens of a single round (content+reasoning+args+envelope), excluding
// system/schema global overhead — those two are request-level constants counted only once in the tail total, so per-round
// accumulation must use marginal estimation (otherwise each round re-counts 400+schema, systematically overestimating,
// tail never meets budget). Used by selectTailByTokens for accumulation.
func estimateRoundTokens(round []miniagent.Message) int {
	return policy.EstimateTokens(round, "", nil) - policy.SystemOverheadTokens
}

// contextKeepRecent is the default number of most recent rounds retained by compactHistory/compactWithSummary.
// P3: lowered from 6 to 4 — reduces steady-state footprint; earlier rounds carried by summary (which already contains
// key facts); the config hook cfg.Run.ContextKeepRecent remains, can be raised back for long ReAct scenarios.
const contextKeepRecent = 4

// §P1-E retainedTail token budget bounds and denominator (ported from opencode compaction.ts:33-34/80-85
// MIN/MAX_PRESERVE_RECENT_TOKENS). tailBudgetFraction=4 corresponds to opencode's usable*0.25 at 1/4 (here using ContextWindow/4).
const (
	minPreserveRecentTokens = 2000
	maxPreserveRecentTokens = 8000
	tailBudgetFraction      = 4
)

// contextKeepReasoning is the default number of most recent assistant messages retained during proactive reasoning cleanup (P1).
// 1 = retain only the most recent assistant's chain-of-thought (current reasoning context), earlier ones cleared. Kept conservative
// because reasoning is large and retained verbatim; long-chain cross-step reasoning tasks that need more reasoning context should
// raise run.context_keep_reasoning (2–3) — token cost grows with each step kept.
const contextKeepReasoning = 1

// contextKeepReasoningChars is the upper character limit (P7, in runes) for a single Reasoning within the retained window.
// When exceeded, head/tail split truncation is applied (reusing text.TruncateHeadTail's head 1/4 + tail 3/4), compressing
// away the divergent middle of overly long reasoning from thinking models while preserving both ends.
// 4000 runes ≈ 2000 tokens: only reasoning chains >4000 are truncated; the vast majority of short reasoning is unaffected.
// 0=unconfigured falls back to this default; config run.context_keep_reasoning_chars set to negative disables, positive customizes threshold.
const contextKeepReasoningChars = 4000

// contextKeepToolArgs is the default number of most recent assistant messages whose args are preserved during proactive tool_call args compression (P4).
// write/edit's content/old/new_string has already been persisted or replaced after successful writes; non-recent N assistant messages' large args
// are compressed to prefix placeholders (only modifying the context-side copy, not breaking pairing). 2 = preserve the write args of the most recent 2 assistants,
// covering the "files being edited" context window; high-accuracy scenarios can tune higher via config run.context_keep_tool_args.
const contextKeepToolArgs = 2

// toolArgsCompressThreshold / toolArgsKeepChars are built-in thresholds for P4 write-args compression:
// compress only when the field exceeds threshold (rune), compress to prefix keepChars (rune) + ellipsis marker; path is always preserved.
// Balance: threshold too low compresses short edits, losing model continuity; keepChars too low loses file header context (e.g. import/package declaration).
const (
	toolArgsCompressThreshold = 600
	toolArgsKeepChars         = 200
)
