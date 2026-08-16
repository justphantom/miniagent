package main

import (
	"os"
	"path/filepath"
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
	for _, want := range []string{"read", "grep", "go build", "golangci-lint", "edit", "replace_all", "Verify", "test"} {
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

// injectSubagentGuidance is auto-only (the fork channel is shell, registered in auto mode only);
// default and empty mode do not inject — the guidance would point at a tool that fails dispatch.
func TestInjectSubagentGuidance_AutoOnly(t *testing.T) {
	const base = "SYS"
	out := injectSubagentGuidance(base, "", "/abs/miniagent.json", "auto")
	if !strings.Contains(out, "-mode auto") {
		t.Errorf("auto not propagated:\n%s", out)
	}
	if !strings.HasPrefix(out, base) {
		t.Errorf("system prompt should be prefix, got: %s", out)
	}
	for _, mode := range []string{"default", ""} {
		if got := injectSubagentGuidance(base, "", "/abs/miniagent.json", mode); got != base {
			t.Errorf("mode %q should not inject (no shell to fork with), got: %s", mode, got)
		}
	}
	// Empty configAbsPath is not injected.
	if got := injectSubagentGuidance(base, "", "", "auto"); got != base {
		t.Errorf("empty config path should not inject, got: %s", got)
	}
}

// NEW-1 regression: under the default config (empty base), assembleSystemPrompt must fall back to
// defaultSystemPrompt, and the final prompt contains the ReAct constraints; otherwise injectSubagentGuidance
// appends to the empty string, leaving the system guidance-only — loopCfg uses resolved.System verbatim (no
// re-default), so the agent would silently lose all workflow constraints.
func TestAssembleSystemPrompt_DefaultApplied(t *testing.T) {
	got := assembleSystemPrompt("", "", "/abs/miniagent.json", "default", "", "")
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

// appendProjectRules: a bare filename under workdir is appended; a missing file (and empty rulesFile/workdir) is a silent no-op.
func TestAppendProjectRules_AppendsAndSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project rule\nPrefer early returns."), 0o600); err != nil {
		t.Fatal(err)
	}
	got := appendProjectRules("BASE", dir, "AGENTS.md")
	if !strings.HasPrefix(got, "BASE") || !strings.Contains(got, "Project rule") {
		t.Errorf("existing rules file should be appended: %q", got)
	}
	if got := appendProjectRules("BASE", dir, "MISSING.md"); got != "BASE" {
		t.Errorf("missing rules file should be a no-op, got: %q", got)
	}
	if got := appendProjectRules("BASE", dir, ""); got != "BASE" {
		t.Errorf("empty rulesFile should be a no-op, got: %q", got)
	}
	if got := appendProjectRules("BASE", "", "AGENTS.md"); got != "BASE" {
		t.Errorf("empty workdir should be a no-op, got: %q", got)
	}
}

// appendProjectRules rejects non-basename rules_file (separators / absolute / "." / "..") — never resolves outside workdir.
func TestAppendProjectRules_RejectsNonBasename(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"docs/rules.md", "/etc/passwd", "..", ".", "a/b"} {
		if got := appendProjectRules("BASE", dir, bad); got != "BASE" {
			t.Errorf("rules_file %q should be rejected (non-basename), got: %q", bad, got)
		}
	}
}

// appendWorkdirLine: a non-empty workdir is injected as an absolute-path line; empty workdir is a no-op.
func TestAppendWorkdirLine_InjectsAndSkipsEmpty(t *testing.T) {
	got := appendWorkdirLine("BASE", "/abs/work")
	if !strings.HasPrefix(got, "BASE") || !strings.Contains(got, "/abs/work") {
		t.Errorf("workdir should be injected: %q", got)
	}
	if got := appendWorkdirLine("BASE", ""); got != "BASE" {
		t.Errorf("empty workdir should be a no-op, got: %q", got)
	}
}

// assembleSystemPrompt layers base (default fallback or config) + rules file + subagent guidance in that order.
// Uses mode=auto because guidance injection is auto-only (shell fork channel).
func TestAssembleSystemPrompt_LayersRulesThenGuidance(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("RULE-TEXT"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := assembleSystemPrompt("", "", "/abs/miniagent.json", "auto", dir, "AGENTS.md")
	if !strings.Contains(got, "Observe before acting") {
		t.Errorf("default base lost: %q", got)
	}
	idxRule := strings.Index(got, "RULE-TEXT")
	idxGuidance := strings.Index(got, "-config ")
	if idxRule < 0 || idxGuidance < 0 {
		t.Fatalf("rules or guidance missing: %q", got)
	}
	if idxRule >= idxGuidance {
		t.Errorf("rules must precede guidance: rule@%d guidance@%d", idxRule, idxGuidance)
	}
	idxWd := strings.Index(got, "Working directory (absolute): "+dir)
	if idxWd < 0 {
		t.Fatalf("workdir line missing: %q", got)
	}
	if idxRule >= idxWd || idxWd >= idxGuidance {
		t.Errorf("order must be rules < workdir < guidance: rule@%d workdir@%d guidance@%d", idxRule, idxWd, idxGuidance)
	}
	got2 := assembleSystemPrompt("MYBASE", "", "/abs/miniagent.json", "auto", dir, "AGENTS.md")
	if !strings.HasPrefix(got2, "MYBASE") || !strings.Contains(got2, "RULE-TEXT") {
		t.Errorf("custom base should also get rules appended: %q", got2)
	}
}
