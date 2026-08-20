package miniagent

import (
	"context"
	"net/http"
	"testing"
)

// OnStep fires once at the top of each iteration, covering every step entered, carrying loop-local state (transcript len,
// token totals, LLMRequests). Observe-only (no error return) — it can never terminate the loop. The snapshot is taken BEFORE
// the step's LLM call, so LLMRequests reflects prior steps only.
func TestRun_OnStepFiresEachIteration(t *testing.T) {
	tool := Tool{Name: "t", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "tr"} }}
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusOK, body: toolResponse(ToolCall{ID: "c", Name: "t", Args: "{}"})},
		{status: http.StatusOK, body: textResponse("done")},
	}}
	llm := testClients(tr)
	var snaps []StepSnapshot
	hooks := LoopHooks{OnStep: func(_ context.Context, s StepSnapshot) { snaps = append(snaps, s) }}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 5}, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if len(snaps) != 2 {
		t.Fatalf("OnStep fired %d times, want 2 (one per entered step): %+v", len(snaps), snaps)
	}
	if snaps[0].Step != 1 || snaps[1].Step != 2 {
		t.Errorf("steps = [%d,%d], want [1,2]", snaps[0].Step, snaps[1].Step)
	}
	// transcript grows across steps (user prompt -> +assistant+tool).
	if snaps[1].TranscriptLen <= snaps[0].TranscriptLen {
		t.Errorf("transcript should grow: %d -> %d", snaps[0].TranscriptLen, snaps[1].TranscriptLen)
	}
	// step1 snapshot is pre-call (LLMRequests=0); step2 reflects step1's call (LLMRequests=1).
	if snaps[0].LLMRequests != 0 {
		t.Errorf("step1 pre-call LLMRequests = %d, want 0", snaps[0].LLMRequests)
	}
	if snaps[1].LLMRequests != 1 {
		t.Errorf("step2 LLMRequests = %d, want 1 (step1's call)", snaps[1].LLMRequests)
	}
}

// OnStep=nil (minimal mode) = zero overhead: the loop runs normally, no panics.
func TestRun_OnStepNilSafe(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{{status: http.StatusOK, body: textResponse("done")}}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m"}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
}
