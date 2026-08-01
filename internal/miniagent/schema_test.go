package miniagent

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// object() 在不传 required 时必须省略 required 键，而非序列化成 null。
// OpenAI 等严格后端会对 "required":null 返回 400。
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
	// 反序列化校验：required 键应不存在。
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["required"]; ok {
		t.Errorf("required key present: %v", got["required"])
	}
}

// object() 传 required 时应输出非空字符串数组。
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

// 所有内置工具的 Parameters 序列化后，required 字段不得为 null。
func TestAllToolSchemas_RequiredNeverNull(t *testing.T) {
	workdir := t.TempDir()
	tools := []Tool{
		ReadFileTool(workdir),
		WriteFileTool(workdir),
		EditFileTool(workdir),
		ShellTool(workdir, 0, ModeAuto),
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
		// required 缺失合规（等同空数组）；存在但为 null 违规。
		if v, ok := schema["required"]; ok {
			if v == nil {
				t.Errorf("%s: required is null (causes LLM 400): %s", tk.Name, b)
			}
			if _, isArr := v.([]any); !isArr {
				t.Errorf("%s: required not array: %T (%s)", tk.Name, v, b)
			}
		}
		// 顺带兜底：原始 JSON 文本不得出现 "required":null。
		if strings.Contains(string(b), `"required":null`) {
			t.Errorf("%s: raw JSON has required:null: %s", tk.Name, b)
		}
	}
}

// validateConfig 对 provider.key 字面量（非 ${VAR} 形式）输出 stderr 警告，引导
// 用环境变量注入避免机密入文件；不强制拒绝以兼容现有用法（审查 P3-11）。
func TestValidateConfig_PlaintextKeyWarns(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "main", ChatURL: "https://api/v1/chat/completions", Key: "sk-literal-abc123"},
		},
	}
	vErr := validateConfig(cfg)
	os.Stderr = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if vErr != nil {
		t.Fatalf("validateConfig: %v", vErr)
	}
	if !strings.Contains(string(out), "明文") || !strings.Contains(string(out), "main") {
		t.Errorf("expected plaintext key warning mentioning provider main, got: %s", out)
	}
}

// ${VAR} 形式的 key（注入式）不应触发警告（审查 P3-11）。
func TestValidateConfig_InjectedKeyNoWarn(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "main", ChatURL: "https://api/v1/chat/completions", Key: "${MAIN_API_KEY}"},
		},
	}
	vErr := validateConfig(cfg)
	os.Stderr = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if vErr != nil {
		t.Fatalf("validateConfig: %v", vErr)
	}
	if strings.Contains(string(out), "明文") {
		t.Errorf("injected ${VAR} key should not warn, got: %s", out)
	}
}

// validateConfig 拒绝 ThinkingMapping.Field 指向保留 payload key：buildChatBody 用
// payload[field]=val 写思考级别（wire.go），命中保留 key（如 max_tokens）会 clobber
// 标准字段（审查 v3 P3）。
func TestValidateConfig_ThinkingFieldBlacklisted(t *testing.T) {
	for _, bad := range []string{"max_tokens", "tools", "messages", "temperature", "model"} {
		cfg := &Config{
			Providers: []ProviderConfig{{
				Name:     "main",
				ChatURL:  "https://api/v1/chat/completions",
				Thinking: &ThinkingMapping{Field: bad},
			}},
		}
		err := validateConfig(cfg)
		if err == nil {
			t.Errorf("field %q: expected error, got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "保留") {
			t.Errorf("field %q: error should mention reserved key, got: %v", bad, err)
		}
	}
}

// 非保留 key（reasoning/thinking/extended_thinking）通过；reasoning_effort 虽是默认
// field 但属保留（显式 mapping 视同误配），应拒绝。
func TestValidateConfig_ThinkingFieldValid(t *testing.T) {
	for _, ok := range []string{"reasoning", "thinking", "extended_thinking"} {
		cfg := &Config{
			Providers: []ProviderConfig{{
				Name:     "main",
				ChatURL:  "https://api/v1/chat/completions",
				Thinking: &ThinkingMapping{Field: ok},
			}},
		}
		if err := validateConfig(cfg); err != nil {
			t.Errorf("field %q should pass, got: %v", ok, err)
		}
	}
	// reasoning_effort 是默认 field（wire.go），显式 mapping 视同误配，应拒绝。
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:     "main",
			ChatURL:  "https://api/v1/chat/completions",
			Thinking: &ThinkingMapping{Field: "reasoning_effort"},
		}},
	}
	if err := validateConfig(cfg); err == nil {
		t.Errorf("reasoning_effort is reserved (default field), should reject explicit mapping")
	}
}
