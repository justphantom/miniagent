package main

import (
	"strings"
	"testing"
)

// shellSingleQuote must wrap a path containing spaces in single quotes (preventing shell word splitting from
// breaking the subagent fork command), and escape embedded single quotes.
func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		"/abs/miniagent.json":      `'/abs/miniagent.json'`,
		"/Users/First Last/x.json": `'/Users/First Last/x.json'`,
		`/a/b's/c.json`:            `'/a/b'\''s/c.json'`,
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
	// A path containing spaces must be wrapped in quotes after subagentGuidance (L3-3).
	if got := renderSubagentGuidance("", "/Users/First Last/x.json", "default"); !strings.Contains(got, "-config '/Users/First Last/x.json'") {
		t.Errorf("config path with spaces not quoted: %s", got)
	}
}
func TestDefaultSystemPrompt_CoversWorkflow(t *testing.T) {
	for _, want := range []string{"read", "grep", "shell", "edit", "replace_all", "Verify", "test"} {
		if !strings.Contains(defaultSystemPrompt, want) {
			t.Errorf("default prompt missing %q", want)
		}
	}
}

// subagentGuidance default mode=default: the fork command contains -mode default and the config path,
// and is marked stateless (session is not persisted).
func TestSubagentGuidance_DefaultMode(t *testing.T) {
	got := renderSubagentGuidance("", "/abs/miniagent.json", "default")
	if !strings.Contains(got, "-mode default") {
		t.Errorf("default mode: expected -mode default in:\n%s", got)
	}
	if !strings.Contains(got, "/abs/miniagent.json") {
		t.Errorf("missing config path:\n%s", got)
	}
	if !strings.Contains(got, "stateless") {
		t.Errorf("missing stateless hint:\n%s", got)
	}
}

// When the parent session mode=auto it is passed through: the fork command should contain -mode auto,
// not the hardcoded default (review v3 P3).
func TestSubagentGuidance_AutoMode(t *testing.T) {
	got := renderSubagentGuidance("", "/abs/miniagent.json", "auto")
	if !strings.Contains(got, "-mode auto") {
		t.Errorf("auto mode: expected -mode auto in:\n%s", got)
	}
	if strings.Contains(got, "-mode default") {
		t.Errorf("auto mode leaked hardcoded default:\n%s", got)
	}
}

// injectSubagentGuidance passes through mode; empty falls back to default (consistent with resolved.Mode fallback);
// empty configAbsPath is not injected (review v3 P3).
func TestInjectSubagentGuidance_PassesMode(t *testing.T) {
	const base = "SYS"
	out := injectSubagentGuidance(base, "", "/abs/miniagent.json", "auto")
	if !strings.Contains(out, "-mode auto") {
		t.Errorf("auto not propagated:\n%s", out)
	}
	if !strings.HasPrefix(out, base) {
		t.Errorf("system prompt should be prefix, got: %s", out)
	}
	// Empty value falls back to default.
	out2 := injectSubagentGuidance(base, "", "/abs/miniagent.json", "")
	if !strings.Contains(out2, "-mode default") {
		t.Errorf("empty mode should fall back to default:\n%s", out2)
	}
	// Empty configAbsPath is not injected.
	if got := injectSubagentGuidance(base, "", "", "auto"); got != base {
		t.Errorf("empty config path should not inject, got: %s", got)
	}
}

// NEW-1 regression: under the default config (empty base), assembleSystemPrompt must fall back to
// defaultSystemPrompt, and the final prompt contains the ReAct constraints; otherwise injectSubagentGuidance
// appends to the empty string making the loopCfg empty-string fallback fail (dead code), and the agent silently
// loses all workflow constraints.
func TestAssembleSystemPrompt_DefaultApplied(t *testing.T) {
	got := assembleSystemPrompt("", "", "/abs/miniagent.json", "default")
	if !strings.Contains(got, "Observe before acting") {
		t.Errorf("under default config system prompt should contain the ReAct constraints from defaultSystemPrompt: %q", got)
	}
}

// renderSubagentGuidance: custom template + placeholder replacement ({config_path} wrapped via shellSingleQuote, {mode} filled directly).
func TestRenderSubagentGuidance_CustomTemplate(t *testing.T) {
	got := renderSubagentGuidance("fork={config_path}|mode={mode}", "/x y/z.json", "auto")
	if want := "fork='/x y/z.json'|mode=auto"; got != want {
		t.Errorf("custom template placeholder replacement = %q, want %q", got, want)
	}
}
