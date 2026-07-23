package miniagent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 每次调用写一条 tool_use 事件，且不含 output 字段。
func TestToolUseWriter(t *testing.T) {
	var buf bytes.Buffer
	emit := ToolUseWriter(&buf)
	if err := emit("read_file", `{"path":"a"}`); err != nil {
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
	if ev["type"] != "tool_use" || ev["name"] != "read_file" {
		t.Errorf("event = %+v", ev)
	}
	if _, ok := ev["output"]; ok {
		t.Errorf("tool_use must not carry output: %+v", ev)
	}
}

func TestEmitResult(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitResult(&buf, Result{Text: "hi", Usage: Usage{InputTokens: 1, OutputTokens: 2}, Steps: 3}, "m"); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "result" || ev["text"] != "hi" || ev["model"] != "m" || ev["steps"] != float64(3) {
		t.Errorf("event = %+v", ev)
	}
}

// 即使所有数值字段为 0，键名也必须出现（消费方稳定 parse 的契约）。
func TestEmitResult_ZeroFieldsPresent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitResult(&buf, Result{}, ""); err != nil {
		t.Fatalf("EmitResult: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"type", "text", "model", "input_tokens", "output_tokens", "steps"} {
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
