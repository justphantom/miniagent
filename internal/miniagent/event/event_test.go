package event

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// 每次调用写一条 tool_use 事件，且不含 output 字段。
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
	if err := EmitResult(&buf, miniagent.Result{Text: "hi", Usage: miniagent.Usage{InputTokens: 1, OutputTokens: 2}, Steps: 3, Finish: "stop"}, "m"); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "result" || ev["text"] != "hi" || ev["model"] != "m" || ev["steps"] != float64(3) || ev["finish"] != "stop" {
		t.Errorf("event = %+v", ev)
	}
}

// 即使所有数值字段为 0，键名也必须出现（消费方稳定 parse 的契约）。
func TestEmitResult_ZeroFieldsPresent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitResult(&buf, miniagent.Result{}, ""); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"type", "text", "model", "input_tokens", "output_tokens", "steps", "finish"} {
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

// Type 空时补 "session"，且全字段（id/model/workdir/provider/created）输出——与 jsonl 首行 metadata 同构。
func TestEmitSession(t *testing.T) {
	var buf bytes.Buffer
	meta := miniagent.SessionMeta{
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
		t.Errorf("type = %v, want session (Type 空时应补全)", ev["type"])
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

// 非 shell 工具不输出 exit_code，避免零值 0 被误读为「命令成功」。
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
