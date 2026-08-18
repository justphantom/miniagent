package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// auto mode registers all 14 builtin tools including shell and web.
func TestBuildTools_AutoRegisters14(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, "")
	want := map[string]bool{"read": true, "write": true, "edit": true, "grep": true, "glob": true, "ast": true, "shell": true, "web": true, "git": true, "go": true, "npm": true, "golangci-lint": true, "rename": true, "delete": true}
	got := toolNames(tools)
	if len(tools) != len(want) {
		t.Fatalf("got %d tools %v, want %d (auto: read/write/edit/grep/glob/ast/shell/web/git/go/npm/lint/rename/delete)", len(tools), got, len(want))
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

// default mode registers 13 tools WITHOUT shell (web included by default): a misfired shell call fails dispatch
// with "unknown tool" (loop_tools), not an executed command — the registration gate replaces the old sudo/su denylist.
func TestBuildTools_DefaultRegisters13NoShell(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, "")
	if len(tools) != 13 {
		t.Fatalf("got %d tools %v, want 13 (default: read/write/edit/grep/glob/ast/web/git/go/npm/lint/rename/delete)", len(tools), toolNames(tools))
	}
	if got := toolNames(tools); got["shell"] {
		t.Fatal("default mode must not register shell")
	}
}

// Empty workdir keeps the same mode-dependent counts (workdir is required at the main entry; this is the degenerate unit case).
func TestBuildTools_EmptyWorkdirModeCounts(t *testing.T) {
	if n := len(buildTools("", 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, "")); n != 14 {
		t.Errorf("auto empty workdir: got %d tools, want 14", n)
	}
	if n := len(buildTools("", 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, "")); n != 13 {
		t.Errorf("default empty workdir: got %d tools, want 13", n)
	}
}

// Empty mode resolves to default at the config layer (resolve.go always yields default|auto); an empty string
// passed directly (degenerate caller) must not accidentally register shell.
func TestBuildTools_EmptyModeTreatedAsDefault(t *testing.T) {
	if got := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, "", 0, miniagent.Limits{}, false, false, "")); got["shell"] {
		t.Fatal("empty mode must not register shell (only explicit auto does)")
	}
}

// web registers unconditionally in both modes (v4.7.1 semantics: default-available, SSRF guard inside the tool).
func TestBuildTools_WebAlwaysRegistered(t *testing.T) {
	if got := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, "")); !got["web"] {
		t.Error("default mode: web must register")
	}
	if got := toolNames(buildTools(t.TempDir(), 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, "")); !got["web"] {
		t.Error("auto mode: web must register")
	}
}

func TestBuildTools_FileResultLimitOverride(t *testing.T) {
	dir := t.TempDir()
	byName := map[string]int{}
	for _, tl := range buildTools(dir, 0, 0, 0, 0, miniagent.ModeAuto, 4242, miniagent.Limits{}, false, false, "") {
		byName[tl.Name] = tl.ResultLimit
	}
	for _, name := range []string{"read", "edit"} {
		if byName[name] != 4242 {
			t.Errorf("%s ResultLimit = %d, want 4242", name, byName[name])
		}
	}
	for _, tl := range buildTools(dir, 0, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, "") {
		if tl.Name == "read" && tl.ResultLimit != 8000 {
			t.Errorf("read ResultLimit = %d, want builtin 8000 when limit<=0", tl.ResultLimit)
		}
	}
}

func TestBuildTools_DefaultConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, "")
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

// buildTools plumbs toolOutputDir into the read-only confine exception (§P1-A): a read of a file under that
// directory passes even though it is outside workdir; a read of an arbitrary outside path is still rejected.
func TestBuildTools_DefaultToolOutputReadException(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	toolOut := filepath.Join(out, "tool_1_x.txt")
	if err := os.WriteFile(toolOut, []byte("persisted"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := buildTools(root, 0, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, out)
	byName := map[string]miniagent.Tool{}
	for _, tk := range ts {
		byName[tk.Name] = tk
	}
	r := byName["read"].Call(context.Background(), fmt.Sprintf(`{"path":%q}`, toolOut))
	if r.IsError {
		t.Fatalf("read of persisted tool-output should be allowed: %s", r.Output)
	}
	if !strings.Contains(r.Output, "persisted") {
		t.Errorf("expected persisted content: %s", r.Output)
	}
	other := filepath.Join(filepath.Dir(out), "sibling.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = byName["read"].Call(context.Background(), fmt.Sprintf(`{"path":%q}`, other))
	if !r.IsError {
		t.Error("read outside allow-root should be rejected")
	}
}
