package main

import (
	"testing"
)

// buildTools 无条件注册 4 个工具，与 workdir 是否为空无关。
func TestBuildTools_AlwaysRegisters4(t *testing.T) {
	tools := buildTools(t.TempDir())
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools))
	}
	expect := map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "shell": true}
	for _, tk := range tools {
		if !expect[tk.Name] {
			t.Errorf("unexpected tool %q", tk.Name)
		}
	}
}

// workdir 为空也注册 4 个工具。
func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("")
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools))
	}
}
