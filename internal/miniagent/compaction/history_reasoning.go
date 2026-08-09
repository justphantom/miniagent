package compaction

import (
	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

import (
	"slices"
)

// stripStaleReasoning proactively clears the Reasoning of assistant messages older than the most recent keepN (P1).
// A single reasoning trace from a thinking model is often several times the body text, and replaying it verbatim
// every turn is a hidden token sink; meanwhile the model's next-step decision rarely needs to look back at earlier
// reasoning — the conclusion is already condensed into that turn's assistant body / tool_calls. Reasoning is
// irrelevant to tool_calls/tool pairing (pairing requires a 1:1 tool_calls ↔ tool correspondence), so clearing it
// does not break pairing and loses no visible fact.
//
// Complementary to trimHistoryForContext (passive, clears everything only when the limit is hit): this function is
// proactive, routine, and clears only "the non-recent N", preserving the current reasoning context. It only
// mutates the context-side copy — the input msgs/newMsgs/session are untouched (persistence keeps the original
// reasoning). keepN<0 is treated as 0; when there is no non-empty reasoning to clear, or all of it is inside the
// retention window, it returns as-is (zero-copy).
func stripStaleReasoning(msgs []miniagent.Message, keepN int) []miniagent.Message {
	if keepN < 0 {
		keepN = 0
	}
	nAssistant := 0
	hasReasoning := false
	for _, m := range msgs {
		if m.Role == miniagent.RoleAssistant {
			nAssistant++
			if m.Reasoning != "" {
				hasReasoning = true
			}
		}
	}
	if !hasReasoning || nAssistant <= keepN {
		return msgs // nothing to clear / all inside the retention window
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	// Walk backward, keep the reasoning of the most recent keepN assistants; clear the older ones.
	kept := 0
	for i := range slices.Backward(out) {
		if out[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		out[i].Reasoning = ""
	}
	return out
}

// truncateKeptReasoning (P7) applies head+tail segmented truncation to oversized Reasoning inside the retention
// window (the most recent keepN assistants). Complementary to stripStaleReasoning: the latter clears "non-retained"
// entries (reducing the count), while this trims the per-entry bulk of "retained" entries — a thinking model's
// single Reasoning often reaches thousands to tens of thousands of tokens, the single largest context item;
// stripStaleReasoning's "all-or-nothing" logic means the retained entry is still replayed verbatim in full (the
// reasoning_content field in wire.go), so P1–P4 do not reduce that bulk at all.
//
// It truncates only when Reasoning exceeds threshold (runes), reusing text.TruncateHeadTail's head 1/4 + tail 3/4
// ratio — both ends are high-value (the opening frames the problem, the closing converges to the conclusion/action),
// while the middle is mostly divergent exploration / trial-and-error / self-correction of the lowest reference value.
// threshold<=0 is treated as disabled and returned as-is; when below the threshold, or no oversized entry exists
// inside the retention window, it returns as-is zero-copy. It only mutates the context-side copy — Reasoning is a
// string (immutable), so mutating out[i].Reasoning does not pollute the caller's input/newMsgs/session; it does not
// touch body/tool_calls/pairing.
func truncateKeptReasoning(msgs []miniagent.Message, keepN, threshold int) []miniagent.Message {
	if threshold <= 0 || keepN <= 0 {
		return msgs
	}
	// Pre-scan: walk the retention window backward (the most recent keepN assistants), check for oversized Reasoning.
	kept := 0
	hasOversized := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept >= keepN {
			break
		}
		kept++
		if len([]rune(msgs[i].Reasoning)) > threshold {
			hasOversized = true
			break
		}
	}
	if !hasOversized {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	kept = 0
	for i := range slices.Backward(out) {
		if out[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept >= keepN {
			break
		}
		kept++
		if len([]rune(out[i].Reasoning)) > threshold {
			out[i].Reasoning = text.TruncateHeadTail(out[i].Reasoning, threshold, "…[reasoning middle omitted]")
		}
	}
	return out
}
