package miniagent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamEmitFunc_ToolUseAlways(t *testing.T) {
	var buf bytes.Buffer
	emit := StreamEmitFunc(&buf, false)
	if err := emit(Signal{Kind: SignalToolUse, Name: "read_file", Input: `{"path":"a"}`}); err != nil {
		t.Fatalf("emit tool_use: %v", err)
	}
	if err := emit(Signal{Kind: SignalToolResult, Name: "read_file", Output: "x"}); err != nil {
		t.Fatalf("emit tool_result: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != "tool_use" || ev.Name != "read_file" {
		t.Errorf("event = %+v", ev)
	}
}

func TestStreamEmitFunc_VerboseIncludesResult(t *testing.T) {
	var buf bytes.Buffer
	emit := StreamEmitFunc(&buf, true)
	if err := emit(Signal{Kind: SignalToolUse, Name: "x", Input: "{}"}); err != nil {
		t.Fatalf("emit tool_use: %v", err)
	}
	if err := emit(Signal{Kind: SignalToolResult, Name: "x", Output: "y", IsError: true}); err != nil {
		t.Fatalf("emit tool_result: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != "tool_result" || !ev.IsError {
		t.Errorf("event = %+v", ev)
	}
}

func TestEmitResult(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitResult(&buf, Result{Text: "hi", Usage: Usage{InputTokens: 1, OutputTokens: 2}, Steps: 3}, "m"); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev streamEvent
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != "result" || ev.Text != "hi" || ev.Model != "m" || ev.Steps != 3 {
		t.Errorf("event = %+v", ev)
	}
}

func TestEmitError(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitError(&buf, "boom"); err != nil {
		t.Fatalf("EmitError: %v", err)
	}
	var ev streamEvent
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != "error" || ev.Message != "boom" {
		t.Errorf("event = %+v", ev)
	}
}
