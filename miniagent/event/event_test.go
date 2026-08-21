package event

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// Each call writes a single tool_use event, and it carries no output field.
func TestToolUseWriter(t *testing.T) {
	var buf bytes.Buffer
	emit := ToolUseWriter(&buf)
	if err := emit("read", `{"path":"a"}`); err != nil {
		t.Fatalf("emit: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "tool_use" || ev["name"] != "read" {
		t.Errorf("event = %+v", ev)
	}
	if _, ok := ev["output"]; ok {
		t.Errorf("tool_use must not carry output: %+v", ev)
	}
}

func TestEmitResult(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitResult(&buf, miniagent.Result{Text: "hi", Usage: miniagent.Usage{InputTokens: 1, OutputTokens: 2}, Steps: 3, Finish: "stop", Compacted: true, ThinkingDowngraded: true}, "m"); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "result" || ev["text"] != "hi" || ev["model"] != "m" || ev["steps"] != float64(3) || ev["finish"] != "stop" {
		t.Errorf("event = %+v", ev)
	}
	if ev["compacted"] != true || ev["thinking_downgraded"] != true {
		t.Errorf("compaction/downgrade flags not surfaced: %+v", ev)
	}
}

// Even when all numeric fields are 0, the key names must still appear (a contract for stable consumer parsing).
func TestEmitResult_ZeroFieldsPresent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitResult(&buf, miniagent.Result{}, ""); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"type", "text", "model", "input_tokens", "output_tokens", "steps", "finish", "compacted", "thinking_downgraded"} {
		if _, ok := ev[key]; !ok {
			t.Errorf("missing key %q in %s", key, buf.String())
		}
	}
}

func TestEmitError(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitError(&buf, "boom"); err != nil {
		t.Fatalf("EmitError: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "error" || ev["message"] != "boom" {
		t.Errorf("event = %+v", ev)
	}
}

// When Type is empty it falls back to "session", and all fields (id/model/workdir/provider/created) are emitted — isomorphic to the first jsonl metadata line.
func TestEmitSession(t *testing.T) {
	var buf bytes.Buffer
	meta := session.SessionMeta{
		ID:       "20240105-120000-aabbccddeeff0011",
		Model:    "openai/gpt-4o",
		Workdir:  "/repo",
		Provider: "openai",
		Created:  "2024-01-05T12:00:00Z",
	}
	if err := EmitSession(&buf, meta); err != nil {
		t.Fatalf("EmitSession: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "session" {
		t.Errorf("type = %v, want session (should fall back when Type is empty)", ev["type"])
	}
	if ev["id"] != meta.ID || ev["model"] != meta.Model || ev["workdir"] != meta.Workdir ||
		ev["provider"] != meta.Provider || ev["created"] != meta.Created {
		t.Errorf("event = %+v", ev)
	}
}

func TestEmitToolResult_ShellExitCode(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitToolResult(&buf, "shell", "c1", miniagent.ToolResult{Output: "out", ExitCode: 7}); err != nil {
		t.Fatalf("EmitToolResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "tool_result" || ev["name"] != "shell" || ev["call_id"] != "c1" || ev["exit_code"] != float64(7) {
		t.Errorf("event = %+v", ev)
	}
}

// Non-shell tools must not emit exit_code, so a zero value of 0 is not misread as "command succeeded".
func TestEmitToolResult_NonShellOmitsExitCode(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitToolResult(&buf, "read", "c2", miniagent.ToolResult{Output: "data"}); err != nil {
		t.Fatalf("EmitToolResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := ev["exit_code"]; ok {
		t.Errorf("non-shell must omit exit_code: %s", buf.String())
	}
}

// A denied/validation result of an exec-backed tool never ran a command: ExitCode is the zero value 0
// there, and emitting it would read as is_error:true + exit_code:0 ("succeeded") simultaneously. The
// field must be omitted for such results.
func TestEmitToolResult_ExecToolValidationErrorOmitsExitCode(t *testing.T) {
	for name, code := range map[string]int{"git": 0, "go": 0, "npm": miniagent.ExitCodeNotSet, "golangci-lint": miniagent.ExitCodeNotSet} {
		var buf bytes.Buffer
		if err := EmitToolResult(&buf, name, "c4", miniagent.ToolResult{IsError: true, ExitCode: code, Output: "git: option rejected"}); err != nil {
			t.Fatalf("EmitToolResult: %v", err)
		}
		var ev map[string]any
		if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev["is_error"] != true {
			t.Errorf("%s: is_error = %v, want true", name, ev["is_error"])
		}
		if _, ok := ev["exit_code"]; ok {
			t.Errorf("%s: error result without a trustworthy code must omit exit_code: %s", name, buf.String())
		}
	}
}

// A non-zero exit of an exec-backed command stays a normal (is_error:false) result carrying its code.
func TestEmitToolResult_ExecNonZeroExitKeepsCode(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitToolResult(&buf, "shell", "c5", miniagent.ToolResult{Output: "FAIL", ExitCode: 1}); err != nil {
		t.Fatalf("EmitToolResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["is_error"] != false {
		t.Errorf("is_error = %v, want false (non-zero exit is a command conclusion)", ev["is_error"])
	}
	if ev["exit_code"] != float64(1) {
		t.Errorf("exit_code = %v, want 1", ev["exit_code"])
	}
}

func TestEmitToolResult_TruncatesLongOutput(t *testing.T) {
	var buf bytes.Buffer
	long := strings.Repeat("x", maxToolResultEventChars+50)
	if err := EmitToolResult(&buf, "read", "c3", miniagent.ToolResult{Output: long}); err != nil {
		t.Fatalf("EmitToolResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["truncated"] != true {
		t.Errorf("truncated = %v, want true", ev["truncated"])
	}
}

func TestEmitDelta(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitDelta(&buf, 2, miniagent.DeltaText, "hi"); err != nil {
		t.Fatalf("EmitDelta: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "text_delta" || ev["step"] != float64(2) || ev["text"] != "hi" {
		t.Errorf("event = %+v", ev)
	}
	buf.Reset()
	if err := EmitDelta(&buf, 3, miniagent.DeltaReasoning, "think"); err != nil {
		t.Fatalf("EmitDelta: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "reasoning_delta" {
		t.Errorf("event = %+v", ev)
	}
}

func TestEmitTs(t *testing.T) {
	// Explicit ts is preserved (replay path); omitted ts is stamped >0 (runtime path).
	var buf bytes.Buffer
	if err := EmitToolUse(&buf, "read", `{}`, 42); err != nil {
		t.Fatalf("EmitToolUse: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["ts"] != float64(42) {
		t.Errorf("ts = %v, want 42", ev["ts"])
	}
	for _, emit := range []func() error{
		func() error { return EmitToolUse(&buf, "read", `{}`) },
		func() error { return EmitDelta(&buf, 1, miniagent.DeltaText, "hi") },
		func() error { return EmitToolResult(&buf, "read", "c1", miniagent.ToolResult{Output: "o"}) },
		func() error { return EmitResult(&buf, miniagent.Result{Text: "t"}, "m") },
	} {
		buf.Reset()
		if err := emit(); err != nil {
			t.Fatalf("emit: %v", err)
		}
		var ev map[string]any
		if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ts, _ := ev["ts"].(float64); ts <= 0 {
			t.Errorf("event ts = %v, want >0 (auto-stamped): %v", ev["ts"], ev["type"])
		}
	}
}
