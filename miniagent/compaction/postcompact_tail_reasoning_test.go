package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// ITEM 2 (post-compaction-tail-reasoning-raw): FitHistory's summarized branch now applies the SAME steady-state
// reasoning strip (stripStaleReasoning + truncateKeptReasoning, with the budget-resolved keepReasoning /
// keepReasoningChars — NOT a hardcoded keepN) to the TAIL subslice only. head + summaryMsg are left untouched. This
// removes the one oversized post-compaction request that replayed the retained tail's full reasoning verbatim — the
// most context-starved request of the run — with zero steady-state divergence (next turn's P1 would clear it anyway).

// Post-compaction out whose tail has more than keepReasoning (=contextKeepReasoning=1) assistant entries: the
// non-recent tail reasoning is stripped, the most-recent tail reasoning is kept, and head + summaryMsg are untouched.
func TestFitHistory_PostCompactionTailReasoningStripped(t *testing.T) {
	bigSummary := strings.Repeat("摘", 5000)
	budget := ContextBudget{
		ContextWindow:   5120,
		SummaryMaxChars: 5000,
		Model:           "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return bigSummary, miniagent.Usage{}, nil
		},
	}
	// splitRounds makes each non-tool message its own round, so rounds = [head, 20×历, A(R1), q1, A(R2), q2]; the
	// retained tail (keepRecent=4) is [A(R1), q1, A(R2), q2] — two assistants with reasoning.
	var msgs []miniagent.Message
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "head"})
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R1", Reasoning: "thinking1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R2", Reasoning: "thinking2"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q2"})

	out, _, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if !summarized {
		t.Fatal("expected compaction (summarized=true) so the post-compaction tail is exercised")
	}

	// head round untouched: out[0] is the retained head user message.
	if out[0].Content != "head" {
		t.Errorf("head round should be retained untouched at out[0], got %+v", out[0])
	}
	// summaryMsg untouched: it is present and precedes the tail.
	tailStart := summaryTailStart(out)
	if tailStart < 0 {
		t.Fatal("expected a KindSummary boundary (the new summaryMsg) in out")
	}
	if !strings.Contains(out[tailStart-1].Content, bigSummary) {
		t.Errorf("summaryMsg (immediately before the tail) should carry the new summary text, got %q", out[tailStart-1].Content)
	}

	// Tail assistants: the most-recent keepReasoning (=1) keeps its reasoning; any older tail assistant is stripped.
	var tailAssistants []miniagent.Message
	for _, m := range out[tailStart:] {
		if m.Role == miniagent.RoleAssistant {
			tailAssistants = append(tailAssistants, m)
		}
	}
	if len(tailAssistants) < 2 {
		t.Fatalf("expected >=2 tail assistants to exercise the steady-state strip, got %d (out=%+v)", len(tailAssistants), out)
	}
	if got := tailAssistants[len(tailAssistants)-1].Reasoning; got != "thinking2" {
		t.Errorf("most-recent tail assistant should KEEP its reasoning, got %q", got)
	}
	for i, m := range tailAssistants[:len(tailAssistants)-1] {
		if m.Reasoning != "" {
			t.Errorf("older tail assistant #%d reasoning should be STRIPPED, got %q", i, m.Reasoning)
		}
	}
}
