package miniagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// safeCall sets ToolResult.ExitCode to ExitCodeNotSet when a tool panics — consistent with denied/unknown tools,
// preventing the event layer from misreading a shell tool panic with exit_code=0 as command success (P3-4 same type).
func TestSafeCall_PanicExitCodeNotSet(t *testing.T) {
	tool := Tool{Name: "shell", Call: func(context.Context, string) ToolResult { panic("boom") }}
	res := safeCall(context.Background(), nil, tool, "shell", "{}")
	if !res.IsError {
		t.Error("panic should IsError=true")
	}
	if res.ExitCode != ExitCodeNotSet {
		t.Errorf("panic ExitCode = %d, want %d (not fully executed, prevents event layer misreading 0 as success)", res.ExitCode, ExitCodeNotSet)
	}
}

// OnToolResult fires once after each tool execution, passing through name/call_id and the result (including IsError).
func TestRun_OnToolResultFired(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "res"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	var got []string
	hooks := LoopHooks{
		OnToolResult: func(step int, name, callID string, r ToolResult) error {
			got = append(got, name+":"+callID+":"+r.Output)
			return nil
		},
	}
	if _, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0] != "q:c1:res" {
		t.Errorf("OnToolResult calls = %v, want [q:c1:res]", got)
	}
}

// OnToolResult returning an error is propagated up the chain to terminate the loop (same semantics as OnToolUse:
// stop when the downstream cannot be written).
func TestRun_OnToolResultErrorStops(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	stop := errors.New("downstream closed")
	hooks := LoopHooks{OnToolResult: func(int, string, string, ToolResult) error { return stop }}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
}

// OnToolUse returns ErrToolDenied: that tool is rejected (a rejection result is backfilled), not executed; other tools run normally.
func TestRun_ToolDeniedSkipped(t *testing.T) {
	toolA := Tool{Name: "a", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "A_ran"} }}
	toolB := Tool{Name: "b", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "B_ran"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "ca", Name: "a", Args: "{}"},
			ToolCall{ID: "cb", Name: "b", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	hooks := LoopHooks{OnToolUse: func(step int, name, callID, input string) error {
		if name == "a" {
			return ErrToolDenied
		}
		return nil
	}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{toolA, toolB}}, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var aOut, bOut string
	for _, m := range res.Messages {
		if m.Role != RoleTool {
			continue
		}
		if m.ToolCallID == "ca" {
			aOut = m.Content
		}
		if m.ToolCallID == "cb" {
			bOut = m.Content
		}
	}
	if !strings.Contains(aOut, "rejected") {
		t.Errorf("denied tool a should be rejected: %q", aOut)
	}
	if !strings.Contains(bOut, "B_ran") {
		t.Errorf("tool b should still run: %q", bOut)
	}
}

// P2-1: when OnToolResult errors midway, each id in assistant.tool_calls must have a corresponding tool message,
// ensuring Messages pairing is complete and resumable (the endpoint will not return 400 due to an orphan tool_call).
func TestRun_OnToolResultErrorKeepsPairing(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "c0", Name: "q", Args: "{}"},
			ToolCall{ID: "c1", Name: "q", Args: "{}"},
			ToolCall{ID: "c2", Name: "q", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	stop := errors.New("downstream closed")
	hooks := LoopHooks{OnToolResult: func(step int, name, callID string, r ToolResult) error {
		if callID == "c1" {
			return stop // interrupt downstream at the 2nd tool
		}
		return nil
	}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
	// Verify each assistant.tool_call id has a matching tool message.
	var wantIDs []string
	gotIDs := make(map[string]bool)
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				wantIDs = append(wantIDs, tc.ID)
			}
		}
		if m.Role == RoleTool {
			gotIDs[m.ToolCallID] = true
		}
	}
	if len(wantIDs) == 0 {
		t.Fatalf("no assistant tool_calls found; msgs=%+v", res.Messages)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("tool_call %q has no matching tool message (pairing broken); msgs=%+v", id, res.Messages)
		}
	}
}

// P3-4: unknown tool's ToolResult.ExitCode should be ExitCodeNotSet (same as denied tools);
// a zero value of 0 would be misread by the event layer as "successful exit".
func TestRun_UnknownToolExitCodeNotSet(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "missing", Args: "{}"}),
		textResponse("ok"),
	}}
	llm := testClients(tr)
	var got *int
	hooks := LoopHooks{OnToolResult: func(step int, name, callID string, r ToolResult) error {
		if name == "missing" {
			ec := r.ExitCode
			got = &ec
		}
		return nil
	}}
	if _, err := Run(context.Background(), llm, LoopConfig{}, "x", hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got == nil {
		t.Fatal("OnToolResult not fired for missing tool")
	}
	if *got != ExitCodeNotSet {
		t.Errorf("unknown tool ExitCode = %d, want %d (ExitCodeNotSet)", *got, ExitCodeNotSet)
	}
}

// panicTransport panics inside RoundTrip, simulating the realistic case of Do/DoStream response parsing paths
// (testParseChatResponse / parseSSE) panicking on malformed payloads.
type panicTransport struct{ called bool }

func (p *panicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	p.called = true
	panic("boom")
}

// P3 LLM panic fallback: callLLMOnce's recover turns a panic in the LLM call path into an error, avoiding a single
// bad response crashing the process (symmetric with safeCall which guards against tool panics). Without the fallback
// this case would panic the entire test process.
func TestRun_LLMCallPanicRecovered(t *testing.T) {
	tr := &panicTransport{}
	llm := testClients(tr)
	_, err := Run(context.Background(), llm, LoopConfig{Model: "m"}, "x", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error from recovered LLM panic, got nil")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("error should surface panic: %v", err)
	}
	if !tr.called {
		t.Error("transport was not invoked")
	}
}

// ShapeToolResult hook overrides the built-in default shaping: the returned content goes directly into history,
// bypassing trimForHistory.
func TestRun_ShapeToolResultOverridesContent(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "raw-output"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	hooks := LoopHooks{ShapeToolResult: func(name, callID string, step int, r ToolResult) (string, error) {
		return "SHAPED:" + r.Output, nil
	}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.ToolCallID == "c1" {
			if m.Content != "SHAPED:raw-output" {
				t.Errorf("tool content = %q, want SHAPED:raw-output", m.Content)
			}
			return
		}
	}
	t.Fatal("no tool message for c1")
}

// ShapeToolResult returning an empty string = core falls back to built-in default shaping (trimForHistory,
// short output passed through as-is).
func TestRun_ShapeToolResultEmptyFallsBack(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "raw-output"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	hooks := LoopHooks{ShapeToolResult: func(string, string, int, ToolResult) (string, error) { return "", nil }}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.ToolCallID == "c1" {
			if m.Content != "raw-output" {
				t.Errorf("tool content = %q, want raw-output (default shaping)", m.Content)
			}
			return
		}
	}
	t.Fatal("no tool message for c1")
}

// ShapeToolResult returning an error is propagated up the chain to terminate the loop, and each id in assistant.tool_calls
// still has a matching tool message (pairing complete).
func TestRun_ShapeToolResultErrorStopsKeepsPairing(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "c0", Name: "q", Args: "{}"},
			ToolCall{ID: "c1", Name: "q", Args: "{}"},
			ToolCall{ID: "c2", Name: "q", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	stop := errors.New("shape failed")
	hooks := LoopHooks{ShapeToolResult: func(name, callID string, step int, r ToolResult) (string, error) {
		if callID == "c1" {
			return "", stop
		}
		return "", nil
	}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
	var wantIDs []string
	gotIDs := make(map[string]bool)
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				wantIDs = append(wantIDs, tc.ID)
			}
		}
		if m.Role == RoleTool {
			gotIDs[m.ToolCallID] = true
		}
	}
	if len(wantIDs) == 0 {
		t.Fatalf("no assistant tool_calls found; msgs=%+v", res.Messages)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("tool_call %q has no matching tool message (pairing broken)", id)
		}
	}
}

// REG-1 regression: after ShapeToolResult errors and OnToolResult is notified for i+1.., if OnToolResult errors
// again at j, placeholders must be backfilled starting from i+1 (not j), otherwise i+1..j-1 lack paired tool messages.
// The original L13 implementation mistakenly used j, breaking pairing.
func TestRun_ShapeToolResultThenOnToolResultErrorKeepsPairing(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "c0", Name: "q", Args: "{}"},
			ToolCall{ID: "c1", Name: "q", Args: "{}"},
			ToolCall{ID: "c2", Name: "q", Args: "{}"},
			ToolCall{ID: "c3", Name: "q", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	shapeErr := errors.New("shape failed")
	onErr := errors.New("onresult failed")
	hooks := LoopHooks{
		ShapeToolResult: func(name, callID string, step int, r ToolResult) (string, error) {
			if callID == "c1" {
				return "", shapeErr
			}
			return "", nil
		},
		OnToolResult: func(step int, name, callID string, r ToolResult) error {
			if callID == "c3" {
				return onErr
			}
			return nil
		},
	}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, onErr) {
		t.Fatalf("err = %v, want %v (OnToolResult error returned with priority)", err, onErr)
	}
	var wantIDs []string
	gotIDs := make(map[string]bool)
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				wantIDs = append(wantIDs, tc.ID)
			}
		}
		if m.Role == RoleTool {
			gotIDs[m.ToolCallID] = true
		}
	}
	if len(wantIDs) != 4 {
		t.Fatalf("want 4 tool_calls, got %d (%+v)", len(wantIDs), wantIDs)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("tool_call %q lacks a paired tool message (REG-1: should backfill placeholder from i+1, not j)", id)
		}
	}
}

// OnToolUse returns a non-ErrToolDenied error: each id in assistant.tool_calls must still have a paired tool message (placeholder).
// Aligned with the OnToolResult error path (P2-1). Regression: previously this path returned directly without backfilling a
// placeholder, breaking the pairing invariant.
func TestRun_OnToolUseErrorKeepsPairing(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "c0", Name: "q", Args: "{}"},
			ToolCall{ID: "c1", Name: "q", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	stop := errors.New("downstream closed")
	hooks := LoopHooks{OnToolUse: func(step int, name, callID, input string) error { return stop }}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
	var wantIDs []string
	gotIDs := make(map[string]bool)
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				wantIDs = append(wantIDs, tc.ID)
			}
		}
		if m.Role == RoleTool {
			gotIDs[m.ToolCallID] = true
		}
	}
	if len(wantIDs) == 0 {
		t.Fatalf("no assistant tool_calls found; msgs=%+v", res.Messages)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("OnToolUse error path: tool_call %q lacks a paired tool message (should backfill placeholder)", id)
		}
	}
}
