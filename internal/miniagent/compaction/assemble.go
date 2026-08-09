// assemble.go: summary prompts, compaction hook, and the NewCompaction assembly entry point.

package compaction

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

// summaryPrefix is the presentation-layer prefix for the Content of persisted miniagent.KindSummary messages (originally hardcoded as "[Previous Conversation Summary]\n").
// Note: identifying a miniagent.KindSummary must use miniagent.Message.Kind == miniagent.KindSummary (applyCompactionBarrier context.go:164),
// not prefix string sniffing — historically there was a space variant "[Previous Conversation Summary] " (non-persist path) inconsistent with test fixtures.
// The prefix is presentation-only and does not participate in identification.
const summaryPrefix = "[Previous Conversation Summary]\n"

// summaryCreateInstruction is the CREATE mode role instruction; the {max_chars} placeholder is replaced by buildSummarizerSystem.
// Users can override via config defaults.summary_create_instruction (placeholder {max_chars}).
const summaryCreateInstruction = "You are a conversation compactor. Compress the following conversation history into an anchored summary of no more than {max_chars} characters, strictly following the template structure below."

// summaryUpdateInstruction is the UPDATE mode role instruction (ported from opencode buildPrompt compaction.ts:164
// preserve/remove/merge). buildSummarizerSystem appends a <previous-summary> block + template after it.
const summaryUpdateInstruction = "You are a conversation compactor. Based on the following conversation history, update the existing anchored summary, output no more than {max_chars} characters. Preserve still-valid facts, remove outdated details, merge in new facts. Use the old summary as an anchor to update:"

// summaryTemplate is a fixed 6-section Markdown template (ported from opencode SUMMARY_TEMPLATE compaction.ts(core):16-46,
// localized to match existing prompt style). Appended after the instruction in both CREATE/UPDATE modes.
const summaryTemplate = `Strictly follow the Markdown structure below, preserving section order. Do not output the <template> tag.
<template>
## Goal
- [What the user wants to accomplish, one or two sentences]

## Key Details
- [Constraints/preferences, decisions and rationale, important facts/assumptions, exact context needed to resume, or "(none)"]

## Progress
### Completed
- [Work done, verified facts, changes made, or "(none)"]

### In Progress
- [Current work, unfinished changes, items under investigation, or "(none)"]

### Blocked
- [Blockers, failed commands, unknowns, or "(none)"]

## Next Step
1. [Immediate concrete action to take, or "(none)"]
2. [If known, the next action, or "(none)"]

## Relevant Files
- [File or directory path: why it matters, or "(none)"]
</template>

Rules:
- Preserve every section, outputting "(none)" even when empty.
- Use concise bullet points, do not write paragraphs.
- Known file paths, symbols, commands, error strings, URLs, and identifiers must be preserved verbatim.
- Do not mention the summary/compaction process itself.`

// buildSummarizerSystem centrally builds the summary system prompt (replacing the old inline system selection). Rules:
//   - summarizerPrompt non-empty → override: the render helper substitutes {max_chars} and {previous_summary}. If the
//     prompt embeds the literal {previous_summary} placeholder the old summary is substituted inline; otherwise, when
//     previousSummary is non-empty, the same <previous-summary> block the default UPDATE path uses is appended
//     unconditionally — so the override is a strict superset of default and never loses the old summary. The
//     summaryTemplate is NOT appended in the override branch (too invasive — a custom prompt may bake in its own
//     template); existing custom prompts without the placeholder strictly improve (was: old summary lost entirely).
//   - Default path + previousSummary non-empty → UPDATE (summaryUpdateInstruction + <previous-summary>
//     block wrapping the old summary + summaryTemplate): the old summary is no longer re-read and re-written as history,
//     saving half the tokens, and the explicit preserve instruction reduces the chance of losing details.
//   - Default path + previousSummary empty → CREATE (summaryCreateInstruction + summaryTemplate).
func buildSummarizerSystem(summarizerPrompt, previousSummary, createInstr, updateInstr, template string, maxChars int) string {
	render := func(s string) string {
		return strings.NewReplacer("{max_chars}", strconv.Itoa(maxChars), "{previous_summary}", previousSummary).Replace(s)
	}
	if summarizerPrompt != "" {
		// Override path: render the user prompt (substituting {max_chars} and {previous_summary}). If the prompt did
		// not embed the {previous_summary} placeholder, append the same <previous-summary> block the default UPDATE
		// path uses so the old summary is never lost — the override is a strict superset of default. The template is
		// intentionally not appended (a custom prompt may carry its own template).
		rendered := render(summarizerPrompt)
		if !strings.Contains(summarizerPrompt, "{previous_summary}") && previousSummary != "" {
			rendered += "\n<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n"
		}
		return rendered
	}
	if createInstr == "" {
		createInstr = summaryCreateInstruction
	}
	if updateInstr == "" {
		updateInstr = summaryUpdateInstruction
	}
	if template == "" {
		template = summaryTemplate
	}
	if previousSummary != "" {
		return render(updateInstr) +
			"\n<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n" +
			template
	}
	return render(createInstr) + "\n\n" + template
}

// stripSummaryPrefix strips summaryPrefix from miniagent.Message.Content to recover the pure summary text for UPDATE feed-back.
// Primary identification must use Kind==miniagent.KindSummary; this function only strips the prefix when present and returns
// the content as-is when absent (defensive against old/hand-written sessions). Note there is a space variant "[Previous Conversation Summary] "
// in the same codepath; here only the production \n prefix is stripped; under mixed history if the prefix does not match the
// original text is returned directly and the UPDATE instruction handles it (prefix is presentation-only, not used for identification).
func stripSummaryPrefix(content string) string {
	return strings.TrimPrefix(content, summaryPrefix)
}

// summarizeMiddle calls the LLM to compress the middle msgs into a single summary text (without tools). Returns the
// maxChars-truncated summary + the miniagent.Usage of this call (for upstream budget accumulation). Reuses miniagent.ChatClient.Do;
// the caller falls back to lossy compaction based on the error (review v2 #6). When previousSummary is non-empty it uses UPDATE mode
// (decided by buildSummarizerSystem).
func summarizeMiddle(ctx context.Context, llm miniagent.Doer, model, summarizerPrompt, previousSummary, createInstr, updateInstr, template string, maxChars, maxSummaryTokens int, msgs []miniagent.Message) (string, miniagent.Usage, error) {
	if maxSummaryTokens <= 0 {
		maxSummaryTokens = summaryMaxTokens
	}
	if len(msgs) == 0 {
		return "", miniagent.Usage{}, errors.New("no middle to summarize")
	}
	system := buildSummarizerSystem(summarizerPrompt, previousSummary, createInstr, updateInstr, template, maxChars)
	resp, err := llm.Do(ctx, miniagent.Request{
		Model:     model,
		System:    system,
		Messages:  msgs,
		MaxTokens: maxSummaryTokens,
	})
	if err != nil {
		return "", miniagent.Usage{}, err
	}
	// R1 defense (summary path): a length-truncated EMPTY summary — reasoning_content burned the output budget on an
	// intrinsic-reasoning compaction model (CompactionModel falls back to the main model when unset) — would silently
	// persist a degraded summary and accelerate semantic dilution on every subsequent compaction. Surface it as an
	// error so compactWithSummary propagates and FitHistory falls back to lossy compaction (split.go), instead of
	// emitting garbage. Non-empty text (even if truncated) is still kept — TruncateHeadTail below handles it.
	if resp.FinishReason == "length" && strings.TrimSpace(resp.Text) == "" && resp.Reasoning != "" {
		return "", resp.Usage, fmt.Errorf("summary truncated to empty by finish_reason=length (reasoning burned the output budget); falling back to lossy compaction")
	}
	// Head-tail split truncation (head 1/4 + tail 3/4): the actionable parts of the summary ("Next Step", "Relevant Files") are often at the end;
	// pure head truncation would drop these first.
	return text.TruncateHeadTail(strings.TrimSpace(resp.Text), maxChars, "...[summary truncated]"), resp.Usage, nil
}

// deriveSummaryMaxChars resolves the summary character cap (direction A): configured>0 uses the explicit user value (override);
// cw<=0 falls back to the built-in summaryMaxChars (no-window compatibility); otherwise min(summaryMaxChars, cw/summaryCharsPerWindowRatio) —
// small windows adapt automatically, preventing the summary itself from exceeding CW×4/5 and causing termination after compaction
// (boundary of B); large windows (CW≥25000) clamp to the built-in upper bound. Pure function, easy to test; called during NewCompaction
// assembly, maxSummaryTokens derivation and jointTailBudget estimation auto-follow.
//
// Hard boundary: even if the summary is shrunk to minimum, CW<~1536 may still terminate (FitHistory's trailing trim/error fallback).
// The root cause is request-level overhead (SystemOverhead 400 + system prompt + tool schema) + head occupying too high a fraction at
// very small CW, independent of summary size — this is a physical limit, and should not be forced by further shrinking the summary (the
// summary would carry no information); instead it should be documented that "too-small CW is unsupported". This function lowers the
// "compaction does not terminate" CW floor from ~5120 (B only) to ~1536, covering all practically useful small-window models.
func deriveSummaryMaxChars(cw, configured int) int {
	if configured > 0 {
		return configured
	}
	if cw <= 0 {
		return summaryMaxChars
	}
	if c := cw / summaryCharsPerWindowRatio; c < summaryMaxChars {
		return c
	}
	return summaryMaxChars
}

// deriveSummaryMaxTokens resolves the summary output token cap: configured>0 uses the explicit user value (override, backward
// compatible with customization); otherwise derived from maxChars as maxChars/2 (densest CJK calibration, same source as EstimateTokens) —
// chars/2 is exactly the token upper bound needed for "pure CJK summary filling the chars limit", ensuring Chinese summaries are not
// truncated by MaxTokens before chars (the original fixed 1024 was too tight). maxChars<2 (including <=0 defensive) falls back to the
// fallback constant summaryMaxTokens. Pure function, easy to test; called during NewCompaction assembly, so that "only configuring
// summary_max_chars" makes token auto-follow, while explicitly configuring summary_max_tokens still overrides.
func deriveSummaryMaxTokens(maxChars, configured int) int {
	if configured > 0 {
		return configured
	}
	if maxChars < 2 {
		return summaryMaxTokens
	}
	return maxChars / 2
}

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
