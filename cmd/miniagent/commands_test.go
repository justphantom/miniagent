package main

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// plan 模式 + 空 workdir：只应注册 webfetch（read_file 被跳过）。
func TestBuildTools_PlanEmptyWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "plan", workdir: ""})
	names := toolNames(tools)
	if len(names) != 1 || names[0] != "webfetch" {
		t.Errorf("plan+empty workdir = %v, want [webfetch]", names)
	}
}

// plan 模式 + workdir：注册 read_file + webfetch。
func TestBuildTools_PlanWithWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "plan", workdir: t.TempDir()})
	names := toolNames(tools)
	if len(names) != 2 || names[0] != "read_file" || names[1] != "webfetch" {
		t.Errorf("plan+workdir = %v, want [read_file webfetch]", names)
	}
}

// default 模式 + workdir：4 个文件/shell 工具 + webfetch。
func TestBuildTools_DefaultWithWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "default", workdir: t.TempDir()})
	names := toolNames(tools)
	if len(names) != 5 {
		t.Errorf("default+workdir got %d tools: %v", len(names), names)
	}
	expect := map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "shell": true, "webfetch": true}
	for _, n := range names {
		if !expect[n] {
			t.Errorf("unexpected tool %q", n)
		}
	}
}

// default 模式 + 空 workdir：只注册 webfetch。
func TestBuildTools_DefaultEmptyWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "default", workdir: ""})
	names := toolNames(tools)
	if len(names) != 1 || names[0] != "webfetch" {
		t.Errorf("default+empty workdir = %v, want [webfetch]", names)
	}
}

// free 模式即使 workdir 为空也注册全部工具（unrestricted）。
func TestBuildTools_FreeEmptyWorkdir(t *testing.T) {
	tools := buildTools(toolConfig{permission: "free", workdir: ""})
	if len(tools) != 5 {
		t.Errorf("free+empty workdir got %d tools, want 5", len(tools))
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

// stateDir 或 chatID 任一为空，initStores 返回空 stores（全 nil）。
func TestInitStores_EmptyReturnsNil(t *testing.T) {
	s := initStores("", "c1", "m", "/tmp", "default", nil)
	if s.history != nil || s.facts != nil || s.meta != nil {
		t.Errorf("empty stateDir should return nil stores, got %+v", s)
	}
	s = initStores(t.TempDir(), "", "m", "/tmp", "default", nil)
	if s.history != nil || s.facts != nil || s.meta != nil {
		t.Errorf("empty chatID should return nil stores, got %+v", s)
	}
}

// 两者都给值时，三个 store 都应初始化，且 meta 落盘。
func TestInitStores_OpensAllThree(t *testing.T) {
	s := initStores(t.TempDir(), "chat-1", "gpt-4o", "/tmp/x", "default", nil)
	if s.history == nil || s.facts == nil || s.meta == nil {
		t.Fatalf("expected all stores non-nil: %+v", s)
	}
	if got := s.meta.Model("chat-1"); got != "gpt-4o" {
		t.Errorf("meta.Model = %q, want gpt-4o", got)
	}
	if got := s.meta.Directory("chat-1"); got != "/tmp/x" {
		t.Errorf("meta.Directory = %q", got)
	}
	if got := s.meta.Permission("chat-1"); got != "default" {
		t.Errorf("meta.Permission = %q", got)
	}
}

func toolNames(tools []miniagent.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// 条数超过上限时应截断并标注总数，避免 system prompt 膨胀。
func TestFormatFactsForCLI_TruncatesByItemCount(t *testing.T) {
	facts := make([]miniagent.Fact, maxMemoryContextItems+10)
	for i := range facts {
		facts[i] = miniagent.Fact{Key: "k" + itoa(i), Value: "v"}
	}
	got := formatFactsForCLI(facts)
	if !strings.Contains(got, "已显示前") {
		t.Errorf("expected truncation marker, got:\n%s", got)
	}
}

// 总字符超过上限时也应截断。
func TestFormatFactsForCLI_TruncatesByCharLimit(t *testing.T) {
	big := strings.Repeat("x", maxMemoryContextChars)
	facts := []miniagent.Fact{
		{Key: "k1", Value: big},
		{Key: "k2", Value: big},
	}
	got := formatFactsForCLI(facts)
	if !strings.Contains(got, "已显示前") {
		t.Errorf("expected char-limit truncation, got length=%d", len(got))
	}
}

// 少量事实正常输出，无截断标注。
func TestFormatFactsForCLI_NoTruncationWhenSmall(t *testing.T) {
	facts := []miniagent.Fact{{Key: "user.lang", Value: "zh"}}
	got := formatFactsForCLI(facts)
	if strings.Contains(got, "已显示前") {
		t.Errorf("unexpected truncation: %s", got)
	}
	if !strings.Contains(got, "user.lang: zh") {
		t.Errorf("missing fact content: %s", got)
	}
}

// 空列表返回空字符串。
func TestFormatFactsForCLI_Empty(t *testing.T) {
	if got := formatFactsForCLI(nil); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
}

// itoa 是避免引入 strconv 的本地实现（仅测试用）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
