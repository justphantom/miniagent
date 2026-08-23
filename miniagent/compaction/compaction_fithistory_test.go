package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

func TestFitHistory_JointBudgetSavesMidWindow(t *testing.T) {
	bigSummary := strings.Repeat("摘", 5000)
	budget := ContextBudget{
		ContextWindow:   5120,
		SummaryMaxChars: 5000,
		Model:           "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return bigSummary, miniagent.Usage{}, nil
		},
	}
	var msgs []miniagent.Message
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	out, _, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("CW=5120 joint budget should not terminate, err=%v (out=%d msgs, summarized=%v)", err, len(out), summarized)
	}
	if !summarized {
		t.Fatal("expected summary compaction to trigger")
	}
}

func TestFitHistory_PreservesTailReasoningOnCompaction(t *testing.T) {
	bigSummary := strings.Repeat("摘", 5000)
	budget := ContextBudget{
		ContextWindow:   5120,
		SummaryMaxChars: 5000,
		Model:           "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return bigSummary, miniagent.Usage{}, nil
		},
	}
	var msgs []miniagent.Message
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "head"})
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R1", Reasoning: "thinking1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R2", Reasoning: "thinking2"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q2"})
	out, _, _, committed, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	reasoningCnt := 0
	keptReasoning := ""
	for _, m := range out {
		if m.Role == miniagent.RoleAssistant && m.Reasoning != "" {
			reasoningCnt++
			keptReasoning = m.Reasoning
		}
	}
	// Post-compaction-tail strip: the tail applies the same steady-state P1 reasoning strip, keeping only the most-recent
	// keepReasoning (=1) assistant's reasoning and clearing earlier tail reasoning (matches next-turn P1).
	if reasoningCnt != 1 {
		t.Errorf("compaction tail should keep exactly 1 (most-recent) reasoning after the post-compaction strip, actual %d", reasoningCnt)
	}
	if keptReasoning != "thinking2" {
		t.Errorf("most-recent reasoning (R2/thinking2) should survive the strip, kept %q", keptReasoning)
	}
	if !committed {
		t.Error("compaction should set committed=true")
	}
}

func TestFitHistory_NonCompactNotCommitted(t *testing.T) {
	budget := ContextBudget{
		ContextWindow: 128000,
		Model:         "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return "s", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q1"}, {Role: miniagent.RoleUser, Content: "q2"}}
	_, _, _, committed, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if committed {
		t.Error("non-compaction should be committed=false (strip is per-round View only, does not replace transcript)")
	}
}
