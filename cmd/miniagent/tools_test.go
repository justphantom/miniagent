package main

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestBuildTools_AlwaysRegisters10(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{})
	if len(tools) != 10 {
		t.Fatalf("got %d tools, want 10 (7 builtins + 3 todo)", len(tools))
	}
}

func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("", 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{})
	if len(tools) != 10 {
		t.Fatalf("got %d tools, want 10 (7 builtins + 3 todo)", len(tools))
	}
}

// S4: fileResultLimit>0 overrides read/edit's ResultLimit; <=0 keeps the constructor builtin default.
func TestBuildTools_FileResultLimitOverride(t *testing.T) {
	dir := t.TempDir()
	byName := map[string]int{}
	for _, tl := range buildTools(dir, 0, 0, 0, miniagent.ModeAuto, 4242, miniagent.Limits{}) {
		byName[tl.Name] = tl.ResultLimit
	}
	for _, name := range []string{"read", "edit"} {
		if byName[name] != 4242 {
			t.Errorf("%s ResultLimit = %d, want 4242", name, byName[name])
		}
	}
	// <=0: keeps the builtin maxFileResultInHistory (8000).
	for _, tl := range buildTools(dir, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}) {
		if tl.Name == "read" && tl.ResultLimit != 8000 {
			t.Errorf("read ResultLimit = %d, want builtin 8000 when limit<=0", tl.ResultLimit)
		}
	}
}

// After buildTools(default), write tools return IsError for an out-of-bounds path (containing "default mode").
func TestBuildTools_DefaultConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{})
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
