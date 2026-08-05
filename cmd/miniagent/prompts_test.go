package main

import (
	"strings"
	"testing"
)

// defaultSystemPrompt 须覆盖代码向工作流的关键约束，防回归误删。
func TestDefaultSystemPrompt_CoversWorkflow(t *testing.T) {
	for _, want := range []string{"read", "grep", "shell", "edit", "replace_all", "验证", "测试"} {
		if !strings.Contains(defaultSystemPrompt, want) {
			t.Errorf("default prompt missing %q", want)
		}
	}
}

// subagentGuidance 默认 mode=default 时，fork 命令含 -mode default 与 config 路径，
// 且标注无状态（不落盘会话）。
func TestSubagentGuidance_DefaultMode(t *testing.T) {
	got := subagentGuidance("/abs/miniagent.json", "default")
	if !strings.Contains(got, "-mode default") {
		t.Errorf("default mode: expected -mode default in:\n%s", got)
	}
	if !strings.Contains(got, "/abs/miniagent.json") {
		t.Errorf("missing config path:\n%s", got)
	}
	if !strings.Contains(got, "无状态") {
		t.Errorf("missing stateless hint:\n%s", got)
	}
}

// 父会话 mode=auto 时透传：fork 命令应含 -mode auto，而非硬编码 default（审查 v3 P3）。
func TestSubagentGuidance_AutoMode(t *testing.T) {
	got := subagentGuidance("/abs/miniagent.json", "auto")
	if !strings.Contains(got, "-mode auto") {
		t.Errorf("auto mode: expected -mode auto in:\n%s", got)
	}
	if strings.Contains(got, "-mode default") {
		t.Errorf("auto mode leaked hardcoded default:\n%s", got)
	}
}

// injectSubagentGuidance 透传 mode；空值兜底 default（与 resolved.Mode 兜底一致）；
// configAbsPath 空不注入（审查 v3 P3）。
func TestInjectSubagentGuidance_PassesMode(t *testing.T) {
	const base = "SYS"
	out := injectSubagentGuidance(base, "/abs/miniagent.json", "auto")
	if !strings.Contains(out, "-mode auto") {
		t.Errorf("auto not propagated:\n%s", out)
	}
	if !strings.HasPrefix(out, base) {
		t.Errorf("system prompt should be prefix, got: %s", out)
	}
	// 空值兜底 default。
	out2 := injectSubagentGuidance(base, "/abs/miniagent.json", "")
	if !strings.Contains(out2, "-mode default") {
		t.Errorf("empty mode should fall back to default:\n%s", out2)
	}
	// configAbsPath 空不注入。
	if got := injectSubagentGuidance(base, "", "auto"); got != base {
		t.Errorf("empty config path should not inject, got: %s", got)
	}
}
