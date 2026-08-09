package miniagent

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestBuildChatBody_ThinkingLevelWritten(t *testing.T) {
	body, err := testBuildChatBody(Request{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning_effort":"medium"`) {
		t.Errorf("thinking level not written: %s", body)
	}
}

func TestBuildChatBody_ThinkingOffOmitted(t *testing.T) {
	for _, lvl := range []string{"", ThinkingOff} {
		body, _ := testBuildChatBody(Request{Model: "m", ThinkingLevel: lvl})
		if strings.Contains(string(body), "reasoning_effort") {
			t.Errorf("level %q should omit thinking: %s", lvl, body)
		}
	}
}

func TestBuildChatBody_ThinkingMappingOverrides(t *testing.T) {
	req := Request{
		Model:         "m",
		ThinkingLevel: "medium",
		Thinking:      &ThinkingMapping{Field: "effort", Map: map[string]string{"medium": "high"}},
	}
	body, _ := testBuildChatBody(req)
	if !strings.Contains(string(body), `"effort":"high"`) {
		t.Errorf("mapping override not applied: %s", body)
	}
	if strings.Contains(string(body), "reasoning_effort") {
		t.Errorf("default field should be replaced: %s", body)
	}
}

// A 400 carrying the thinking signature → callLLMWithDowngrade retries once with thinking dropped (review v2 #7).
func TestCallLLM_Thinking400Downgrade(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{status: http.StatusOK, body: textResponse("ok")},
	}}
	llm := testClients(tr)
	resp, _, _, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("callLLMWithDowngrade: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	if tr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (downgrade retry)", tr.calls)
	}
	if !strings.Contains(tr.bodies[0], `"reasoning_effort":"medium"`) {
		t.Errorf("first body should carry thinking: %s", tr.bodies[0])
	}
	if strings.Contains(tr.bodies[1], "reasoning_effort") {
		t.Errorf("second body should drop thinking: %s", tr.bodies[1])
	}
}

// A non-thinking 400 does not trigger a downgrade (when no thinking was sent, it propagates directly).
func TestCallLLM_Plain400NoDowngrade(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
	}}
	llm := testClients(tr)
	_, _, _, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m"}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error for plain 400")
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d, want 1 (no downgrade without thinking)", tr.calls)
	}
}

// When the summary step triggers a thinking downgrade, Result.ThinkingDowngraded should be set (same as the
// main path / C retry; before the fix the closure `resp2, _, err2` discarded downgraded, so the interaction
// layer resent the original thinking next turn and hit 400 again).
func TestRun_SummaryStepCapturesDowngrade(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusOK, body: toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})},
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{status: http.StatusOK, body: textResponse("summary done")},
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1, ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "summary done" {
		t.Errorf("Text = %q, want summary done", res.Text)
	}
	if !res.ThinkingDowngraded {
		t.Errorf("ThinkingDowngraded = false, want true (summary-step downgrade should be captured)")
	}
	if tr.calls != 3 {
		t.Errorf("calls = %d, want 3 (step1 tool + summary thinking 400 + summary downgrade ok)", tr.calls)
	}
	// LLMRequests must equal the transport call count: step1 tool (1) + summary step downgrade (2) = 3. Before the
	// fix all three callLLMWithDowngrade sites discarded the `requests` return and did a flat llmReqs++, so a
	// downgrade undercounted (here 2 instead of 3) and the cross-turn meta.LLMRequests accumulation drifted.
	if res.LLMRequests != 3 {
		t.Errorf("LLMRequests = %d, want 3 (downgrade retry must count: 1 main + 2 summary-downgrade)", res.LLMRequests)
	}
}

// Main-path thinking downgrade: the LLM rejects reasoning_effort (400) then succeeds with it dropped. The downgrade
// retry sends 2 actual requests, so Result.LLMRequests must be 2 — the flat llmReqs++ at the main call site
// previously undercounted this as 1 (loop_api.go documents LLMRequests as "including thinking downgrade retry").
func TestRun_MainPathDowngrade_CountsRequests(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{status: http.StatusOK, body: textResponse("done")},
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want 2", tr.calls)
	}
	if res.LLMRequests != 2 {
		t.Errorf("LLMRequests = %d, want 2 (main-path downgrade: 2 actual requests)", res.LLMRequests)
	}
}

// OnLLMError retry that itself downgrades: main call errors (1 request) → hook retries → retry's first attempt is
// thinking-unsupported → downgrade → ok (2 requests). Total LLMRequests = 3. The OnLLMError-retry call site
// previously did a flat llmReqs++ and would undercount this as 2.
func TestRun_OnLLMErrorRetryDowngrade_CountsRequests(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unrelated upstream error"}}`},            // generic 400 → propagates to OnLLMError (not thinking-unsupported, not context-length)
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`}, // retry attempt 1 → downgrade
		{status: http.StatusOK, body: textResponse("done")},                                                  // retry downgrade ok
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Model: "m", MaxIterations: 5, ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}
	var retried bool
	hooks := LoopHooks{OnLLMError: func(ctx context.Context, step int, msgs []Message, err error) ([]Message, bool, error) {
		if retried {
			t.Fatalf("OnLLMError invoked twice; expected a single retry")
		}
		retried = true
		return msgs, true, nil // retry once, no trim
	}}
	res, err := Run(context.Background(), llm, cfg, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if tr.calls != 3 {
		t.Errorf("calls = %d, want 3 (main error + retry thinking-400 + retry downgrade ok)", tr.calls)
	}
	if res.LLMRequests != 3 {
		t.Errorf("LLMRequests = %d, want 3 (1 main + 2 retry-downgrade)", res.LLMRequests)
	}
}
