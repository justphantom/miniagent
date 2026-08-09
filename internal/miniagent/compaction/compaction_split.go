package compaction

import (
	"github.com/justphantom/miniagent/internal/miniagent/policy"
	"slices"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

import "log/slog"

// CompactionOptions holds the parameters for NewCompaction — it consolidates the context-compaction strategy
// originally scattered across LoopConfig into one place, making compaction a pluggable "add-on" rather than core
// config. Fields <=0/empty fall back to the built-in defaults inside the engine (see context.go).
type CompactionOptions struct {
	// Chat is the client used for summary compaction (may be a different provider than the main chat); when nil
	// the caller must inject Summarize, otherwise NewCompaction builds the summarizeMiddle callback from it.
	Chat miniagent.Doer
	// MaxTokens is the single-turn output limit of the main request, used by isUsageOverflow to detect silent
	// overflow (mirrors the original cfg.MaxTokens).
	MaxTokens int
	// ContextWindow is the model context limit (tokens); <=0 disables proactive compaction (only the
	// ErrContextLength passive retry remains).
	ContextWindow int
	// Model is the main model id; when CompactionModel is empty, this is used as the fallback for summarization.
	Model string
	// CompactionModel is the summary-specific model id (may span providers); empty falls back to Model.
	CompactionModel string
	System          string
	Tools           []miniagent.Tool
	KeepRecent      int // compactWithSummary retains this many recent rounds (<=0 uses the built-in default).
	// KeepReasoning/KeepToolArgs/KeepReasoningChars are proactive trimming params (P1/P4/P7, <=0/0 uses the built-in default).
	KeepReasoning      int
	KeepToolArgs       int
	KeepReasoningChars int
	SummarizerPrompt   string // Non-empty fully overrides the summary system prompt.
	// SummaryCreateInstruction/SummaryUpdateInstruction are the summary instruction templates (CREATE/UPDATE),
	// placeholder {max_chars}; SummaryTemplate is the fixed summary structure template (no variables). Empty uses
	// the built-in default. Passed through via config defaults.*.
	SummaryCreateInstruction string
	SummaryUpdateInstruction string
	SummaryTemplate          string
	SummaryMaxChars          int
	// SummaryMaxTokens caps the summary request output tokens; <=0 (default) NewCompaction derives it from
	// SummaryMaxChars (chars/2, the densest CJK ratio), so a Chinese summary is not truncated by MaxTokens before
	// reaching chars. Explicit >0 overrides the derivation.
	SummaryMaxTokens     int
	PreserveRecentTokens int
	UseRealUsage         bool
	// Auto toggles silent usage-overflow detection (AfterLLM samples real usage to judge overflow and sets the
	// next step's Force compaction); false=off.
	Auto     bool
	Reserved int // Reserved is the token buffer reserved from ContextWindow (<=0 falls back to the built-in default).
	// SessionID is passed through to OnCompacting (CompactingInput.SessionID); empty string tolerates no-session.
	SessionID string
	// OnCompacting is the pre-summary hook (inject context / one-shot replace summarizerPrompt); nil=disabled.
	OnCompacting CompactingHook
	Logger       *slog.Logger
}

// This file is the "history tools" portion of the compaction subsystem (split out from history_util.go), used
// only by the compaction engine:
// splitRounds/flatten/compactHistory/trimRecentRounds (round splitting and lossy trimming),
// the estimateTokensFromUsage family (real-usage-first + staleness-aware threshold estimation),
// isUsageOverflow/usableTokens/compactionReserve (silent overflow detection).
// Together with context.go / history_*.go it belongs to the compaction cluster, all relocated to internal/miniagent/compaction.

// estimateMessageTokensLocal estimates the tokens of a single message locally (CJK≈1/2, others≈1/4), counting
// Content+Reasoning+ToolCalls.Args + the marginal overhead of adding this message to the request (envelope
// EnvelopePerMsgTokens + tool_call wrapping EnvelopePerToolCallTokens).
// It does not count the global system/schema overhead (a request-level constant, covered by EstimateTokens'
// SystemOverheadTokens fallback).
// Previously the envelope/tool_call wrapping was undercounted, causing estimateTokensFromUsage to systematically
// underestimate on long sessions (envelope accumulates), so compaction fired too late.
func estimateMessageTokensLocal(m miniagent.Message) int {
	var nonCJK, cjk int
	n, c := text.CountCharsLocal(m.Content)
	nonCJK, cjk = nonCJK+n, cjk+c
	n, c = text.CountCharsLocal(m.Reasoning)
	nonCJK, cjk = nonCJK+n, cjk+c
	for _, tc := range m.ToolCalls {
		n, c = text.CountCharsLocal(tc.Args)
		nonCJK, cjk = nonCJK+n, cjk+c
	}
	return nonCJK/4 + cjk/2 + policy.EnvelopePerMsgTokens + policy.EnvelopePerToolCallTokens*len(m.ToolCalls)
}

// contextTokensFromUsage converts a single response's usage into "the total tokens of that request's prefix + output".
// miniagent has no cache field; InputTokens already includes the full OpenAI prompt_tokens (including cache hits).
func contextTokensFromUsage(u miniagent.Usage) int {
	return u.InputTokens + u.OutputTokens
}

// lastApplicableUsageIndex returns the index of the most recent assistant whose usage "applies to the current
// prefix", or -1 if none. Anti-staleness check: only miniagent.KindSummary (summary) redefines the prefix; take
// the last assistant with Ts>=latestSummaryTs and non-zero usage.
func lastApplicableUsageIndex(msgs []miniagent.Message) int {
	var latestSummaryTs int64
	for _, m := range msgs {
		if m.Kind == miniagent.KindSummary && m.Ts > latestSummaryTs {
			latestSummaryTs = m.Ts
		}
	}
	idx := -1
	for i, m := range msgs {
		if m.Role == miniagent.RoleAssistant && m.Usage != nil &&
			(m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0) &&
			m.Ts >= latestSummaryTs {
			idx = i
		}
	}
	return idx
}

// estimateTokensFromUsage is a two-stage estimate: lastApplicableUsageIndex finds the most recent non-stale
// real-usage anchor, tokens = contextTokensFromUsage(usage) + Σ estimateMessageTokensLocal(msgs[idx+1:]).
// When ok=false (no anchor) the caller falls back to policy.EstimateTokens.
func estimateTokensFromUsage(msgs []miniagent.Message) (tokens int, ok bool) {
	idx := lastApplicableUsageIndex(msgs)
	if idx < 0 {
		return 0, false
	}
	tokens = contextTokensFromUsage(*msgs[idx].Usage)
	for _, m := range msgs[idx+1:] {
		tokens += estimateMessageTokensLocal(m)
	}
	return tokens, true
}

// estimateThreshold wraps the "real usage first, fall back to local" strategy for a single FitHistory call site.
func estimateThreshold(msgs []miniagent.Message, system string, tools []miniagent.Tool, useRealUsage bool) int {
	if useRealUsage {
		if t, ok := estimateTokensFromUsage(msgs); ok {
			return t
		}
	}
	return policy.EstimateTokens(msgs, system, tools)
}

// compactionBuffer is the token buffer reserved for model output and future-round growth (mirrors opencode COMPACTION_BUFFER=20000).
const compactionBuffer = 20000

// usageFootprint returns the real context footprint (input+output) of a single LLM call.
func usageFootprint(u miniagent.Usage) int {
	return contextTokensFromUsage(u)
}

// compactionReserve returns the number of tokens reserved from ContextWindow (mirrors reserved in opencode usable()).
func compactionReserve(maxTokens, reservedCfg int) int {
	if reservedCfg > 0 {
		return reservedCfg
	}
	if maxTokens > 0 {
		return min(compactionBuffer, maxTokens)
	}
	return compactionBuffer
}

// usableTokens returns the token budget available for history filling (mirrors opencode usable()).
// reserve is clamped to at most CW/5: prevents compactionReserve (default min(20000,maxTokens)) from taking more
// than half of CW on small CW, which would make the isUsageOverflow (usage>=usable) threshold lower than the
// FitHistory gate (CW*4/5=80%), letting the Force path take over too early.
func usableTokens(contextWindow, maxTokens, reservedCfg int) int {
	if contextWindow <= 0 {
		return 0
	}
	reserve := min(compactionReserve(maxTokens, reservedCfg), contextWindow/5)
	return max(0, contextWindow-reserve)
}

// isUsageOverflow determines whether the previous step's real usage has hit the context window (mirrors opencode isOverflow, §P1-B).
func isUsageOverflow(u miniagent.Usage, contextWindow, maxTokens, reservedCfg int, auto bool) bool {
	if !auto || contextWindow <= 0 {
		return false
	}
	return usageFootprint(u) >= usableTokens(contextWindow, maxTokens, reservedCfg)
}

// One round (grouped, preserving tool_calls/tool pairing); user and assistant without tool_calls each form their own round.
func splitRounds(msgs []miniagent.Message) [][]miniagent.Message {
	var rounds [][]miniagent.Message
	var cur []miniagent.Message
	flush := func() {
		if len(cur) > 0 {
			rounds = append(rounds, cur)
			cur = nil
		}
	}
	for _, m := range msgs {
		if m.Role == miniagent.RoleTool && len(cur) > 0 {
			cur = append(cur, m) // tool belongs to the currently open assistant(tool_calls) round
			continue
		}
		flush()
		cur = []miniagent.Message{m}
		if len(m.ToolCalls) == 0 {
			flush() // user / pure assistant: independent round
		}
	}
	flush()
	return rounds
}

func flatten(rounds [][]miniagent.Message) []miniagent.Message {
	var out []miniagent.Message
	for _, r := range rounds {
		out = append(out, r...)
	}
	return out
}

// compactHistory is the lossy fallback when summarization fails or there is no middle segment: retains
// "the earliest 1 round + the most recent keepRecent rounds".
func compactHistory(msgs []miniagent.Message, keepRecent int) []miniagent.Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs
	}
	out := append([]miniagent.Message{}, rounds[0]...)
	out = append(out, flatten(rounds[len(rounds)-keepRecent:])...)
	return out
}

// trimRecentRounds keeps only the most recent keepRecent rounds; it is the final lossy trim when compactHistory
// still exceeds the window.
func trimRecentRounds(msgs []miniagent.Message, keepRecent int) []miniagent.Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= keepRecent {
		return msgs
	}
	trimmed := flatten(rounds[len(rounds)-keepRecent:])
	// Keep the most recent KindSummary (it may sit in the trimmed head — just generated by a summary call): prepend
	// it to the trim result, otherwise the summary call is wasted + the model loses the middle-segment summary and
	// the initial context. If the trim result already contains a summary, or there is no summary to keep, return as-is.
	var latestSummary miniagent.Message
	hasSummary := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Kind == miniagent.KindSummary {
			latestSummary = msgs[i]
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		return trimmed
	}
	for _, m := range trimmed {
		if m.Kind == miniagent.KindSummary {
			return trimmed
		}
	}
	return append([]miniagent.Message{latestSummary}, trimmed...)
}
