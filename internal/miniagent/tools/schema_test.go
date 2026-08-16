package tools

import (
	"encoding/json"
	"strings"
	"testing"

	miniagent "github.com/justphantom/miniagent/internal/miniagent"
)

// object() must omit the required key when none is passed, rather than serializing it as null.
// Strict backends like OpenAI return a 400 on "required":null.
func TestObject_OmitsRequiredWhenNone(t *testing.T) {
	schema := object(map[string]any{
		"prefix": map[string]any{"type": "string"},
	})
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(b)
	if strings.Contains(raw, `"required"`) {
		t.Errorf("expected required key omitted, got %s", raw)
	}
	if strings.Contains(raw, "null") {
		t.Errorf("null leaked into schema: %s", raw)
	}
	// Deserialize and verify: the required key should be absent.
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["required"]; ok {
		t.Errorf("required key present: %v", got["required"])
	}
}

// object() should emit a non-empty string array when required is passed.
func TestObject_EmitsRequiredWhenGiven(t *testing.T) {
	schema := object(map[string]any{"path": map[string]any{"type": "string"}}, "path")
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req, ok := got["required"].([]any)
	if !ok {
		t.Fatalf("required not an array: %T", got["required"])
	}
	if len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v, want [path]", req)
	}
}

// After serializing the Parameters of all built-in tools, the required field must never be null.
// Covers every constructor registered by cmd/miniagent buildTools (11 default-mode tools + shell),
// so a regression in any object(...) call (e.g. a nil required) fails here.
func TestAllToolSchemas_RequiredNeverNull(t *testing.T) {
	workdir := t.TempDir()
	tools := []miniagent.Tool{
		ReadFileTool(workdir, 0, 0),
		WriteFileTool(workdir, 0),
		EditFileTool(workdir, 0),
		GrepTool(workdir, 0, 0, 0),
		GlobTool(workdir, 0, 0),
		GitTool(workdir, 0, 0),
		GoTool(workdir, 0, 0),
		NpmTool(workdir, 0, 0),
		LintTool(workdir, 0, 0),
		RenameTool(workdir, 0),
		DeleteTool(workdir, 0),
		ShellTool(workdir, 0, 0, 0),
	}

	for _, tk := range tools {
		b, err := json.Marshal(tk.Parameters)
		if err != nil {
			t.Errorf("%s: marshal: %v", tk.Name, err)
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Errorf("%s: unmarshal: %v", tk.Name, err)
			continue
		}
		// A missing required is compliant (equivalent to an empty array); present-but-null is a violation.
		if v, ok := schema["required"]; ok {
			if v == nil {
				t.Errorf("%s: required is null (causes LLM 400): %s", tk.Name, b)
			}
			if _, isArr := v.([]any); !isArr {
				t.Errorf("%s: required not array: %T (%s)", tk.Name, v, b)
			}
		}
		// Belt-and-suspenders: the raw JSON text must not contain "required":null.
		if strings.Contains(string(b), `"required":null`) {
			t.Errorf("%s: raw JSON has required:null: %s", tk.Name, b)
		}
	}
}
