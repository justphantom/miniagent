package compaction

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
