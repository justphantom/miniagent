package miniagent

import (
	"context"
	"errors"
	"testing"
)

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
