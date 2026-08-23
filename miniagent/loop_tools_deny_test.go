package miniagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
