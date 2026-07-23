package main

import (
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// default 模式 + workdir：注册 4 个文件/shell 工具。
func TestBuildTools_DefaultWithWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "default", workdir: t.TempDir()})
	names := toolNames(tools)
	if len(names) != 4 {
		t.Errorf("default+workdir got %d tools: %v", len(names), names)
	}
	expect := map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "shell": true}
	for _, n := range names {
		if !expect[n] {
			t.Errorf("unexpected tool %q", n)
		}
	}
}

// default 模式 + 空 workdir：不注册任何工具。
func TestBuildTools_DefaultEmptyWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "default", workdir: ""})
	if len(tools) != 0 {
		t.Errorf("default+empty workdir = %v, want []", toolNames(tools))
	}
}

// free 模式即使 workdir 为空也注册全部工具（unrestricted）。
func TestBuildTools_FreeEmptyWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "free", workdir: ""})
	if len(tools) != 4 {
		t.Errorf("free+empty workdir got %d tools, want 4", len(tools))
	}
}

// blockedPatterns 透传到 ShellTool（通过行为间接验证：rm -rf 被拦）。
func TestBuildTools_BlockedPatternsPropagated(t *testing.T) {
	tools := buildTools(toolConfig{permission: "default", workdir: t.TempDir(), blockedPatterns: []string{"forbidden-token"}})
	var shell miniagent.Tool
	for _, tk := range tools {
		if tk.Name == "shell" {
			shell = tk
		}
	}
	res := shell.Call(nil, `{"command":"echo forbidden-token"}`)
	if !res.IsError {
		t.Errorf("blocked pattern not propagated: %s", res.Output)
	}
}

// stateDir 或 chatID 任一为空，initHistory 返回 nil（无状态模式）。
func TestInitHistory_EmptyReturnsNil(t *testing.T) {
	if h := initHistory("", "c1", nil); h != nil {
		t.Errorf("empty stateDir should return nil, got %v", h)
	}
	if h := initHistory(t.TempDir(), "", nil); h != nil {
		t.Errorf("empty chatID should return nil, got %v", h)
	}
}

// 两者都给值时，history 应初始化。
func TestInitHistory_OpensHistory(t *testing.T) {
	h := initHistory(t.TempDir(), "chat-1", nil)
	if h == nil {
		t.Fatalf("expected non-nil history")
	}
}

func toolNames(tools []miniagent.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}
