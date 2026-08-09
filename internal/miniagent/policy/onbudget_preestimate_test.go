package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/policy"
)

// st2: on the zero-usage (streaming) path, OnBudget reuses the compaction pass's PreEstimate instead of re-scanning ToSend.
// ToSend is large enough that EstimateTokens would give a value != 555, so total==555 proves PreEstimate was used.
func TestNewDefaultOnBudget_PreEstimateReused(t *testing.T) {
	hook := policy.NewDefaultOnBudget(0, nil)
	total := &miniagent.Usage{}
	in := miniagent.BudgetInput{
		ToSend:      []miniagent.Message{{Role: miniagent.RoleUser, Content: strings.Repeat("x", 10000)}},
		System:      "s",
		Resp:        miniagent.Response{Usage: miniagent.Usage{}}, // zero usage → inZero
		PreEstimate: 555,
	}
	if err := hook(context.Background(), 1, in, total); err != nil {
		t.Fatalf("OnBudget: %v", err)
	}
	if total.InputTokens != 555 {
		t.Errorf("InputTokens = %d, want 555 (PreEstimate reused, not EstimateTokens of the 10k-char ToSend)", total.InputTokens)
	}
}

// Control: PreEstimate=0 (non-compaction path) → OnBudget estimates ToSend itself (the original fallback).
func TestNewDefaultOnBudget_NoPreEstimateEstimates(t *testing.T) {
	hook := policy.NewDefaultOnBudget(0, nil)
	total := &miniagent.Usage{}
	in := miniagent.BudgetInput{
		ToSend:      []miniagent.Message{{Role: miniagent.RoleUser, Content: strings.Repeat("x", 10000)}},
		System:      "s",
		Resp:        miniagent.Response{Usage: miniagent.Usage{}}, // zero usage → inZero
		PreEstimate: 0,                                            // no compaction estimate → must estimate
	}
	if err := hook(context.Background(), 1, in, total); err != nil {
		t.Fatalf("OnBudget: %v", err)
	}
	if total.InputTokens == 0 || total.InputTokens == 555 {
		t.Errorf("InputTokens = %d, want the EstimateTokens result (>0, not 0/555)", total.InputTokens)
	}
}
