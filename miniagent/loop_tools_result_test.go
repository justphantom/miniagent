package miniagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

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
