package main

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func toolNames(tools []miniagent.Tool) map[string]bool {
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	return names
}

// auto mode registers all 13 builtin tools including shell (web stays opt-in).
func TestBuildTools_AutoRegisters13(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false)
	want := map[string]bool{"read": true, "write": true, "edit": true, "grep": true, "glob": true, "ast": true, "shell": true, "git": true, "go": true, "npm": true, "golangci-lint": true, "rename": true, "delete": true}
	got := toolNames(tools)
	if len(tools) != len(want) {
		t.Fatalf("got %d tools %v, want %d (auto: read/write/edit/grep/glob/ast/shell/git/go/npm/lint/rename/delete)", len(tools), got, len(want))
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing expected tool %q (got %v)", name, got)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q registered (got %v)", name, got)
		}
	}
}

// default mode registers 12 tools WITHOUT shell: a misfired shell call fails dispatch with "unknown tool"
// (loop_tools), not an executed command — the registration gate replaces the old sudo/su denylist.
func TestBuildTools_DefaultRegisters12NoShell(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, false)
	if len(tools) != 12 {
		t.Fatalf("got %d tools %v, want 12 (default: read/write/edit/grep/glob/ast/git/go/npm/lint/rename/delete)", len(tools), toolNames(tools))
	}
	if got := toolNames(tools); got["shell"] {
		t.Fatal("default mode must not register shell")
	}
}

// Empty workdir keeps the same mode-dependent counts (workdir is required at the main entry; this is the degenerate unit case).
func TestBuildTools_EmptyWorkdirModeCounts(t *testing.T) {
	if n := len(buildTools("", 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false)); n != 13 {
		t.Errorf("auto empty workdir: got %d tools, want 13", n)
	}
	if n := len(buildTools("", 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, false)); n != 12 {
		t.Errorf("default empty workdir: got %d tools, want 12", n)
	}
}

// Empty mode resolves to default at the config layer (resolve.go always yields default|auto); an empty string
// passed directly (degenerate caller) must not accidentally register shell.
func TestBuildTools_EmptyModeTreatedAsDefault(t *testing.T) {
	if got := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, "", 0, miniagent.Limits{}, false, false, false)); got["shell"] {
		t.Fatal("empty mode must not register shell (only explicit auto does)")
	}
}

// web is opt-in (run.web_fetch): off in both modes by default; on adds exactly one tool.
func TestBuildTools_WebOptIn(t *testing.T) {
	if got := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, false)); got["web"] {
		t.Error("default without web_fetch: web must not register")
	}
	if got := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false)); got["web"] {
		t.Error("auto without web_fetch: web must not register")
	}
	ts := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, true))
	if !ts["web"] {
		t.Error("web_fetch=true (default mode): web must register")
	}
	if n := len(buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, true)); n != 13 {
		t.Errorf("default+web: got %d tools, want 13", n)
	}
}

func TestBuildTools_FileResultLimitOverride(t *testing.T) {
	dir := t.TempDir()
	byName := map[string]int{}
	for _, tl := range buildTools(dir, 0, 0, 0, 0, miniagent.ModeAuto, 4242, miniagent.Limits{}, false, false, false) {
		byName[tl.Name] = tl.ResultLimit
	}
	for _, name := range []string{"read", "edit"} {
		if byName[name] != 4242 {
			t.Errorf("%s ResultLimit = %d, want 4242", name, byName[name])
		}
	}
	for _, tl := range buildTools(dir, 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false) {
		if tl.Name == "read" && tl.ResultLimit != 8000 {
			t.Errorf("read ResultLimit = %d, want builtin 8000 when limit<=0", tl.ResultLimit)
		}
	}
}

func TestBuildTools_DefaultConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, false)
	byName := map[string]miniagent.Tool{}
	for _, tk := range tools {
		byName[tk.Name] = tk
	}
	cases := []struct{ name, args string }{
		{"write", `{"path":"../escape.txt","content":"x"}`},
		{"edit", `{"path":"../escape.txt","old_string":"a","new_string":"b"}`},
		{"edit", `{"path":"../escape.txt","edits":[{"old_string":"a","new_string":"b"}]}`},
	}
	for _, c := range cases {
		r := byName[c.name].Call(context.Background(), c.args)
		if !r.IsError || !strings.Contains(r.Output, "default mode") {
			t.Errorf("%s escape should be rejected: %s", c.name, r.Output)
		}
	}
}
