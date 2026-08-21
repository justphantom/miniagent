package compaction

import (
	"context"
	"errors"
	"strings"

	"github.com/justphantom/miniagent/miniagent"
)

// CompactingInput is a snapshot of the summarization scene, letting an external hook decide whether to inject/replace (§P2, mirroring opencode experimental.session.compacting).
type CompactingInput struct {
	// SessionID is the session id this summary belongs to (injected via CompactionOptions.SessionID; cmd layer passes through from session meta.ID). Empty string is compatible with no-session mode.
	SessionID string
	// Middle is the middle segment to be summarized (cut out by compactWithSummary, including any merged-in old miniagent.KindSummary).
	// Read-only: the hook must not mutate middle in place; injection is done via CompactingOutput.Context appended by applyCompactingHook.
	Middle []miniagent.Message
	// Model is the model id actually used for this summary (already fallen back CompactionModel→Model).
	Model string
}

// CompactingOutput is the hook's return value: inject context or one-time replace summarizerPrompt.
type CompactingOutput struct {
	// Context is extra text appended to the summary input (domain knowledge/file lists/external memory, etc.); empty slice = no injection.
	// applyCompactingHook appends it as a single role=user message at the end of middle, entering the summary input rather than the system
	// channel (aligning with opencode compaction.ts nextPrompt entering the user channel semantics).
	Context []string
	// Prompt, when non-empty, replaces this summary's summarizerPrompt (one-time, does not persistently change budget.SummarizerPrompt).
	// Empty string = keep budget.SummarizerPrompt.
	Prompt string
}

// CompactingHook triggers synchronously before each summary (inside compactWithSummary, before calling budget.Summarize).
// Mirrors opencode experimental.session.compacting: can inject context or replace the prompt, but cannot cancel
// (only pi supports cancel; miniagent does not support it at this stage).
// Contract (implementation A, consistent with opencode plugin.trigger default semantics, see HOOKS.md §2.9): a hook
// throwing an error aborts **this summary** compaction — FitHistory then falls back to lossy compactHistory (does not
// give up compaction entirely, preventing unbounded context growth).
// nil = no hook, applyCompactingHook short-circuits with zero overhead.
type CompactingHook func(ctx context.Context, in CompactingInput) (CompactingOutput, error)

// applyCompactingHook triggers the hook inside compactWithSummary, before calling budget.Summarize (§P2),
// folding the hook output back into (effPrompt, effMiddle): non-empty Prompt overrides this summary's summarizerPrompt; non-empty
// Context is appended as a single role=user message at the end of middle (enters the summary input, not the system channel).
// Contract: hook==nil returns the original prompt/middle directly (zero-overhead short-circuit). A hook returning error → propagated
// up to abort this summary compaction, compactWithSummary returns that error, FitHistory falls back to lossy compactHistory (HOOKS.md §2.9).
// Context injection only appends user messages without tool_calls, not breaking tool pairing (session.ValidateToolPairing has already passed before the call).
func applyCompactingHook(ctx context.Context, hook CompactingHook, sessionID, model, summarizerPrompt string, middle []miniagent.Message) (effPrompt string, effMiddle []miniagent.Message, err error) {
	if hook == nil {
		return summarizerPrompt, middle, nil
	}
	out, err := hook(ctx, CompactingInput{SessionID: sessionID, Middle: middle, Model: model})
	if err != nil {
		return "", nil, err
	}
	effPrompt = summarizerPrompt
	if out.Prompt != "" {
		effPrompt = out.Prompt
	}
	effMiddle = middle
	if len(out.Context) > 0 {
		injected := append(append([]miniagent.Message{}, middle...), miniagent.Message{Role: miniagent.RoleUser, Content: strings.Join(out.Context, "\n")})
		effMiddle = injected
	}
	return effPrompt, effMiddle, nil
}

// NewCompaction wraps the entire context compaction engine (FitHistory: over-window summarize middle + lossy fallback +
// proactive trimming; §P1-B silent overflow detection) into a pair of pluggable LoopHooks hooks. This is the default
// implementation of "compaction as a plugin": the core Run contains no compaction; the caller gets (before, after) via
// NewCompaction(opts) and attaches them to LoopHooks.BeforeLLM/AfterLLM to restore full compaction capability; if not attached,
// a minimal no-compaction agent results. before does applyCompactionBarrier + FitHistory + silent overflow detection each step
// (inferring Force from the real usage in in.Msgs). after is always nil since 4.2.0 (overflow detection merged into before, no
// cross-step state) — the caller must nil-check before attaching AfterLLM. opts.Chat must be non-nil (the summary LLM call needs a client).
func NewCompaction(opts CompactionOptions) (before func(context.Context, miniagent.StepInput) (miniagent.StepOutput, error), after func(context.Context, int, miniagent.Response) error) {
	// summary_max_chars scales with ContextWindow by default (direction A, deriveSummaryMaxChars): min(5000, CW/5),
	// small windows adapt automatically to prevent the summary itself from exceeding CW×4/5 and causing termination after compaction; explicit >0 overrides.
	maxChars := deriveSummaryMaxChars(opts.ContextWindow, opts.SummaryMaxChars)
	// summary_max_tokens is derived from maxChars by default (chars/2, densest CJK calibration): token auto-follows maxChars
	// (including A scaling), ensuring Chinese summaries are not truncated by MaxTokens before chars; explicit >0 overrides.
	maxSummaryTokens := deriveSummaryMaxTokens(maxChars, opts.SummaryMaxTokens)
	budget := ContextBudget{
		ContextWindow:        opts.ContextWindow,
		KeepRecent:           opts.KeepRecent,
		KeepReasoning:        opts.KeepReasoning,
		KeepToolArgs:         opts.KeepToolArgs,
		KeepReasoningChars:   opts.KeepReasoningChars,
		SummarizerPrompt:     opts.SummarizerPrompt,
		CompactionModel:      opts.CompactionModel,
		Model:                opts.Model,
		System:               opts.System,
		Tools:                opts.Tools,
		UseRealUsage:         opts.UseRealUsage,
		PreserveRecentTokens: opts.PreserveRecentTokens,
		SummaryMaxChars:      maxChars,
		Compacting:           opts.OnCompacting,
		EstimateTokens:       opts.EstimateTokens,
		SessionID:            opts.SessionID,
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			if opts.Chat == nil {
				return "", miniagent.Usage{}, errors.New("compaction: Chat is nil, cannot summarize (must configure CompactionOptions.Chat)")
			}
			return summarizeMiddle(ctx, opts.Chat, model, sys, prevSummary, opts.SummaryCreateInstruction, opts.SummaryUpdateInstruction, opts.SummaryTemplate, maxChars, maxSummaryTokens, middle)
		},
	}
	before = func(ctx context.Context, in miniagent.StepInput) (miniagent.StepOutput, error) {
		barrier := applyCompactionBarrier(in.Msgs)
		// §P1-B: detect silent overflow from the previous step's already-historized assistant.Usage (compact before hitting the provider).
		// Each step infers Force into a local variable, then copies budget to pass into FitHistory — the closure does not write shared
		// state, so concurrent reuse of the same hook by multiple Runs is race-free (ContextBudget is a value type; b and budget share
		// only read-only references like Summarize/Tools).
		force := false
		if idx := lastApplicableUsageIndex(in.Msgs); idx >= 0 {
			force = isUsageOverflow(*in.Msgs[idx].Usage, opts.ContextWindow, opts.Reserved, opts.Auto)
		}
		b := budget
		b.Force = force
		fitted, summary, summarized, committed, sumUsage, viewEstimate, err := FitHistory(ctx, barrier, b, opts.Logger)
		if err != nil {
			return miniagent.StepOutput{}, err
		}
		out := miniagent.StepOutput{View: fitted, Commit: committed, ViewEstimate: viewEstimate} // only compaction/fallback replaces transcript; non-compaction strip is this round's View only. viewEstimate (0 on non-compaction) lets OnBudget skip a duplicate EstimateTokens scan on the streaming zero-usage path (st2).
		if summarized {
			out.Persist = []miniagent.Message{summary}
			u := sumUsage
			out.ExtraUsage = &u
			out.Compacted = true
		}
		return out, nil
	}
	// after no longer holds cross-step state: overflow detection moved into before, inferred from in.Msgs. Return nil so the caller skips the no-op AfterLLM.
	after = nil
	return before, after
}
