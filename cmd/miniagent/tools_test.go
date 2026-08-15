package main

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestBuildTools_AlwaysRegisters8(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false, nil)
	want := map[string]bool{"read": true, "write": true, "edit": true, "grep": true, "glob": true, "shell": true, "git": true, "go": true}
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl.Name] = true
	}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools %v, want %d (read/write/edit/grep/glob/shell/git/go)", len(tools), got, len(want))
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

func TestBuildTools_EmptyWorkdirStillRegisters8(t *testing.T) {
	tools := buildTools("", 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false, nil)
	if len(tools) != 8 {
		t.Fatalf("got %d tools, want 8 (read/write/edit/grep/glob/shell/git/go)", len(tools))
	}
}

func TestBuildTools_FileResultLimitOverride(t *testing.T) {
	dir := t.TempDir()
	byName := map[string]int{}
	for _, tl := range buildTools(dir, 0, 0, 0, miniagent.ModeAuto, 4242, miniagent.Limits{}, false, false, false, nil) {
		byName[tl.Name] = tl.ResultLimit
	}
	for _, name := range []string{"read", "edit"} {
		if byName[name] != 4242 {
			t.Errorf("%s ResultLimit = %d, want 4242", name, byName[name])
		}
	}
	for _, tl := range buildTools(dir, 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false, false, nil) {
		if tl.Name == "read" && tl.ResultLimit != 8000 {
			t.Errorf("read ResultLimit = %d, want builtin 8000 when limit<=0", tl.ResultLimit)
		}
	}
}

func TestBuildTools_DefaultConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, 0, 0, miniagent.ModeDefault, 0, miniagent.Limits{}, false, false, false, nil)
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
