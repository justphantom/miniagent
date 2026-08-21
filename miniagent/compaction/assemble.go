// assemble.go: summary prompts, compaction hook, and the NewCompaction assembly entry point.

package compaction

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/text"
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

// proseOnlyRetryDirective is appended to the system prompt on the single retry after summary garbage detection:
// the retry is prompt-level (not temperature/param fiddling) so tool-shaped responses cannot pass the check again.
const proseOnlyRetryDirective = "\n\nYour previous output was REJECTED: it was not prose summary text (it contained tool-call markup or raw code). OUTPUT PROSE ONLY: the summary text itself, no <tool_call> tags, no code blocks, no tool invocations."

// summaryGarbageMarkers are output shapes that can never be a legitimate summary — a summarizer that echoes tool-call
// markup is producing an executable draft, not a summary (see the corrupted-summary incident: such output persisted as
// KindSummary becomes a prompt-injection vector on resume). Structural check, prefix-free.
// Note: a code fence is NOT in the list — summaries legitimately quote multi-line commands and error strings verbatim
// (the built-in template demands it), so an in-body fence is only rejected when it is the summary's shape (leading
// fence, see firstLineIsCodeFence); blanket rejection misfired on legal CJK technical summaries and forced an
// unnecessary lossy fallback.
var summaryGarbageMarkers = []string{"<tool_call>", "<tool_calls>", "</tool_call>"}

// firstLineIsCodeFence reports whether t opens with a code fence (first non-blank line starts with ```): such output
// is a code block with no prose — an executable draft, not a summary. An in-body fence (commands/errors quoted inside
// an otherwise prose summary) is legitimate and passes.
func firstLineIsCodeFence(t string) bool {
	for line := range strings.Lines(t) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return strings.HasPrefix(strings.TrimSpace(line), "```")
	}
	return false
}

// isSummaryGarbage reports whether t is structurally unfit to persist as a KindSummary: containing tool-call markup,
// opening with a bare code fence, or (when the built-in summarizer shape is in effect) lacking ≥2 of the template
// section headings. A custom summarizerPrompt (config defaults, CompactOnly option, or CompactingHook override),
// custom instructions, or a custom template disables the section check — only markup rejection applies there, since a
// custom prompt may bake in its own structure.
func isSummaryGarbage(t, summarizerPrompt, createInstr, updateInstr, template string) bool {
	for _, m := range summaryGarbageMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	if firstLineIsCodeFence(t) {
		return true
	}
	if summarizerPrompt != "" || createInstr != "" || updateInstr != "" || template != "" {
		return false
	}
	sections := 0
	// Count heading LINES (prefix match per line), not substrings: "## Goal" buried inside a paragraph must not count.
	for line := range strings.Lines(t) {
		l := strings.TrimSpace(line)
		for _, h := range []string{"## Goal", "## Key Details", "## Progress", "## Next Step", "## Relevant Files"} {
			if strings.HasPrefix(l, h) && len(l) > len(h) {
				sections++
				break
			}
		}
	}
	return sections < 2
}

// summarizeMiddle calls the LLM to compress the middle msgs into a single summary text (without tools). Returns the
// maxChars-truncated summary + the miniagent.Usage of this call (for upstream budget accumulation). Reuses miniagent.ChatClient.Do;
// the caller falls back to lossy compaction based on the error (review v2 #6). When previousSummary is non-empty it uses UPDATE mode
// (decided by buildSummarizerSystem). Garbage outputs (tool-call markup / template-less text on the default template) are rejected and
// retried once with a strict prose-only directive; a second failure surfaces as an error → FitHistory falls back to lossy compaction.
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
		return "", resp.Usage, errors.New("summary truncated to empty by finish_reason=length (reasoning burned the output budget); falling back to lossy compaction")
	}
	// P0 (corrupted summary injection): a non-empty response can still be structurally unfit (tool-call markup / code / missing the
	// template sections the consumer expects). Retry ONCE with a strict prose-only directive; a second failure surfaces as an error so
	// FitHistory falls back to lossy compaction. Both attempts' usage is accumulated (like the length/429 downgrade path).
	if isSummaryGarbage(resp.Text, summarizerPrompt, createInstr, updateInstr, template) {
		retry, rerr := llm.Do(ctx, miniagent.Request{
			Model:     model,
			System:    system + proseOnlyRetryDirective,
			Messages:  msgs,
			MaxTokens: maxSummaryTokens,
		})
		resp.Usage.InputTokens += retry.Usage.InputTokens
		resp.Usage.OutputTokens += retry.Usage.OutputTokens
		if rerr != nil {
			return "", resp.Usage, rerr
		}
		if isSummaryGarbage(retry.Text, summarizerPrompt, createInstr, updateInstr, template) {
			return "", resp.Usage, errors.New("summary output is not prose (tool-call markup / code / missing template sections after prose-only retry); falling back to lossy compaction")
		}
		resp.Text = retry.Text
	}
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
