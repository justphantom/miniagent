package main

import (
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

// Single permission mode: 8 builtin tools (read/write/edit/grep/glob/ast/shell/web).
func TestBuildTools_Registers8(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, 0, 0, miniagent.Limits{})
	want := map[string]bool{"read": true, "write": true, "edit": true, "grep": true, "glob": true, "ast": true, "shell": true, "web": true}
	got := toolNames(tools)
	if len(tools) != len(want) {
		t.Fatalf("got %d tools %v, want %d", len(tools), got, len(want))
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

func TestBuildTools_FileResultLimitOverride(t *testing.T) {
	dir := t.TempDir()
	byName := map[string]int{}
	for _, tl := range buildTools(dir, 0, 0, 0, 0, 4242, miniagent.Limits{}) {
		byName[tl.Name] = tl.ResultLimit
	}
	for _, name := range []string{"read", "edit"} {
		if byName[name] != 4242 {
			t.Errorf("%s ResultLimit = %d, want 4242", name, byName[name])
		}
	}
	for _, tl := range buildTools(dir, 0, 0, 0, 0, 0, miniagent.Limits{}) {
		if tl.Name == "read" && tl.ResultLimit != 8000 {
			t.Errorf("read ResultLimit = %d, want builtin 8000 when limit<=0", tl.ResultLimit)
		}
	}
}
