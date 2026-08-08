package main

import (
	"strings"
	"testing"
)

// shellSingleQuote 须把含空格的路径单引号包裹（防 shell 词法拆断 subagent fork 命令），并转义内嵌单引号。
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
	// 含空格的路径经 subagentGuidance 后须被引号包裹（L3-3）。
	if got := renderSubagentGuidance("", "/Users/First Last/x.json", "default"); !strings.Contains(got, "-config '/Users/First Last/x.json'") {
		t.Errorf("含空格的 config 路径未被引用: %s", got)
	}
}
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
	got := renderSubagentGuidance("", "/abs/miniagent.json", "default")
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
	got := renderSubagentGuidance("", "/abs/miniagent.json", "auto")
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
	out := injectSubagentGuidance(base, "", "/abs/miniagent.json", "auto")
	if !strings.Contains(out, "-mode auto") {
		t.Errorf("auto not propagated:\n%s", out)
	}
	if !strings.HasPrefix(out, base) {
		t.Errorf("system prompt should be prefix, got: %s", out)
	}
	// 空值兜底 default。
	out2 := injectSubagentGuidance(base, "", "/abs/miniagent.json", "")
	if !strings.Contains(out2, "-mode default") {
		t.Errorf("empty mode should fall back to default:\n%s", out2)
	}
	// configAbsPath 空不注入。
	if got := injectSubagentGuidance(base, "", "", "auto"); got != base {
		t.Errorf("empty config path should not inject, got: %s", got)
	}
}

// NEW-1 回归：默认配置（空 base + 无项目规则）下 assembleSystemPrompt 须兜底 defaultSystemPrompt，
// 最终 prompt 含 ReAct 约束；否则 injectSubagentGuidance 向空串追加使 loopCfg 空串 fallback 失效（死代码），
// agent 静默丢失全部工作流约束。
func TestAssembleSystemPrompt_DefaultApplied(t *testing.T) {
	got := assembleSystemPrompt("", projectRules{}, "", "/abs/miniagent.json", "default")
	if !strings.Contains(got, "先观察后动手") {
		t.Errorf("默认配置下 system prompt 应含 defaultSystemPrompt 的 ReAct 约束: %q", got)
	}
	// persona 取代默认 prompt（优先级 persona>defaults）
	got = assembleSystemPrompt("", projectRules{persona: "你是专属助手"}, "", "/abs/miniagent.json", "default")
	if !strings.Contains(got, "你是专属助手") {
		t.Errorf("persona 应进入 prompt: %q", got)
	}
	if strings.Contains(got, "先观察后动手") {
		t.Errorf("persona 应取代默认 prompt（不再含默认约束）: %q", got)
	}
}

// renderSubagentGuidance：自定义模板 + 占位符替换（{config_path} 经 shellSingleQuote 包裹，{mode} 直填）。
func TestRenderSubagentGuidance_CustomTemplate(t *testing.T) {
	got := renderSubagentGuidance("fork={config_path}|mode={mode}", "/x y/z.json", "auto")
	if want := "fork='/x y/z.json'|mode=auto"; got != want {
		t.Errorf("自定义模板占位符替换 = %q, want %q", got, want)
	}
}
