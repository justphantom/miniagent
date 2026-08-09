package miniagent

import (
	"context"
	"errors"
	"testing"
)

// After hitting maxIterations, the summary step (summarizeAtLimit) fallback call goes through recordStepUsage
// and OnBudget (P1-3 fix). Previously the fallback accumulated usage manually, bypassing OnBudget, so the
// degraded path could silently exceed MaxTotalTokens. This test pins the circuit-break closure (a blind spot
// flagged by the regression-review agent: the guard code was correct but lacked a direct test).
func TestRun_SummarizeAtLimitOnBudgetExceeds(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	// 2 main-loop steps + the summary step all return tool_calls (summary-step tool_calls != 0 → the fallback
	// goes through recordStepUsage).
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}), // summary-step fallback
	}}
	llm := testClients(tr)
	// Each step's toolResponse has prompt_tokens=1: the 2 main-loop steps accumulate input=2 < 3 (pass); the
	// summary step accumulates 3 >= 3 → circuit-break.
	hooks := LoopHooks{OnBudget: func(_ context.Context, _ int, _ BudgetInput, total *Usage) error {
		if total.InputTokens >= 3 {
			return ErrBudgetExceeded
		}
		return nil
	}}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 2}, "x", hooks, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("OnBudget inside the summary-step fallback should circuit-break and return ErrBudgetExceeded, got %v", err)
	}
}
