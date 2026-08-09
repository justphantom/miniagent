package miniagent

import (
	"context"
	"strings"
	"testing"
)

// Phase 3: inject summary prompt after iteration limit, LLM returns text → FinishStop, steps=maxIterations+1.
func TestRun_SummaryInjectionSucceeds(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("summary done"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishStop {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishStop)
	}
	if res.Text != "summary done" {
		t.Errorf("Text = %q, want 'summary done'", res.Text)
	}
	if res.Steps != 2 { // 1 tool step + 1 summary step
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
	if tr.calls != 2 {
		t.Errorf("LLM calls = %d, want 2", tr.calls)
	}
	// Second request body should contain the summary request prompt.
	secondBody := tr.bodies[1]
	if !strings.Contains(secondBody, summaryRequestPrompt) {
		t.Errorf("second request missing summary prompt: %s", secondBody)
	}
}

// Phase 3: inject summary prompt after iteration limit, LLM still requests tool → fallback FinishMaxIterations.
func TestRun_SummaryInjectionFallsBack(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		toolResponse(ToolCall{ID: "c2", Name: "loop", Args: "{}"}),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1 (fallback, no extra step accumulated)", res.Steps)
	}
	// summary request is still injected and sent to the LLM (second request body contains the prompt), but no longer pollutes the transcript
	// (Result.Messages does not contain the internal RoleSystem bootstrap message — sent via temporary reqMsgs).
	secondBody := tr.bodies[1]
	if !strings.Contains(secondBody, summaryRequestPrompt) {
		t.Errorf("second request missing summary prompt: %s", secondBody)
	}
	for i, m := range res.Messages {
		if m.Role == RoleSystem {
			t.Errorf("Result.Messages[%d] should not contain internal RoleSystem bootstrap message: %+v", i, m)
		}
	}
}

// Phase 3: inject a custom summary request prompt after iteration limit (configured via LoopConfig).
func TestRun_SummaryRequestPromptConfigurable(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("custom summary"),
	}}
	customPrompt := "this is a custom summary bootstrap"
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{
		Tools:          []Tool{tool},
		MaxIterations:  1,
		SummaryRequest: customPrompt,
	}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "custom summary" {
		t.Errorf("Text = %q, want 'custom summary'", res.Text)
	}
	// Verify the custom prompt was used instead of the default.
	secondBody := tr.bodies[1]
	if !strings.Contains(secondBody, customPrompt) {
		t.Errorf("second request missing custom summary prompt: %s", secondBody)
	}
	if strings.Contains(secondBody, summaryRequestPrompt) {
		t.Errorf("second request should not contain default summary prompt: %s", secondBody)
	}
}

// Summary step LLM call fails (err2, e.g. bad response) → fallback FinishMaxIterations — does not propagate, does not pollute transcript
// (summaryReq goes via temporary reqMsgs, so failure does not enter Messages). Covers the err2 exit path of summarizeAtLimit.
func TestRun_SummaryLLMFailureFallsBack(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		"not-valid-json", // summary step parse failure → err2
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("summary LLM failure should fallback instead of propagating, got: %v", err)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q (summary failure fallback)", res.Finish, FinishMaxIterations)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1 (=iterLimit)", res.Steps)
	}
	for i, m := range res.Messages {
		if m.Role == RoleSystem {
			t.Errorf("Result.Messages[%d] should not contain RoleSystem: %+v", i, m)
		}
	}
}
