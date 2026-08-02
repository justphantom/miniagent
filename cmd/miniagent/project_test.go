package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadProjectRules 读 persona/rules/scripts/memory；各文件可选。
func TestLoadProjectRules_AllSources(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".miniagent", "persona.md"), "你是本项目专属 agent。")
	writeFile(t, filepath.Join(dir, ".miniagent", "rules.md"), "禁止提交未跑测试的代码。")
	writeFile(t, filepath.Join(dir, ".miniagent", "scripts.json"), `{"scripts":[{"name":"test","command":"go test ./...","description":"跑测试"}]}`)
	writeFile(t, filepath.Join(dir, ".miniagent", "memory.jsonl"), `{"type":"lesson","topic":"t","content":"记忆A"}`+"\n")

	pr := loadProjectRules(dir)
	if pr.persona != "你是本项目专属 agent。" {
		t.Errorf("persona = %q", pr.persona)
	}
	if !strings.Contains(pr.rules, "禁止提交") {
		t.Errorf("rules = %q", pr.rules)
	}
	if len(pr.scripts) != 1 || pr.scripts[0].Name != "test" || pr.scripts[0].Command != "go test ./..." {
		t.Errorf("scripts = %+v", pr.scripts)
	}
	if !strings.Contains(pr.memory, "记忆A") {
		t.Errorf("memory snippet should contain 记忆A: %q", pr.memory)
	}
	if !pr.hasAny() {
		t.Error("hasAny should be true")
	}
}

// 缺少 .miniagent 目录 → 零值，hasAny=false（不污染 system prompt）。
func TestLoadProjectRules_EmptyWorkdir(t *testing.T) {
	pr := loadProjectRules(t.TempDir())
	if pr.hasAny() {
		t.Error("empty workdir should have no project rules")
	}
	if pr.scripts != nil {
		t.Errorf("scripts should be nil, got %+v", pr.scripts)
	}
}

// mergeSystemPrompt：persona 取代 base；rules/memory 追加；hasAny 时告知 LLM。
func TestMergeSystemPrompt_PersonaOverridesBase(t *testing.T) {
	got := mergeSystemPrompt("default-workflow", "专属人格", "规则A", "记忆B", true)
	if !strings.HasPrefix(got, "专属人格") {
		t.Errorf("persona should be base: %s", got)
	}
	if !strings.Contains(got, "## 项目规则\n规则A") || !strings.Contains(got, "记忆B") {
		t.Errorf("rules/memory missing: %s", got)
	}
	if !strings.Contains(got, "已加载 .miniagent/") {
		t.Errorf("should note loaded rules: %s", got)
	}
}

// mergeSystemPrompt：无 persona 时沿用 base（工作流约束），仅追加 rules。
func TestMergeSystemPrompt_KeepsBaseWhenNoPersona(t *testing.T) {
	got := mergeSystemPrompt("default-workflow", "", "规则A", "", true)
	if !strings.HasPrefix(got, "default-workflow") {
		t.Errorf("base should be kept: %s", got)
	}
	if !strings.Contains(got, "规则A") {
		t.Errorf("rules missing: %s", got)
	}
}

// buildTools 注册 script_<name> 工具，跳过 name/command 为空者。
func TestBuildTools_RegistersScriptTools(t *testing.T) {
	scripts := []scriptDef{
		{"test", "go test ./...", "跑测试"},
		{"", "echo x", "空 name 跳过"},
		{"lint", "", "空 command 跳过"},
	}
	tools := buildTools(t.TempDir(), 0, miniagent.ModeAuto, 0, scripts)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["script_test"] {
		t.Error("script_test should be registered")
	}
	if names["script_lint"] {
		t.Error("script with empty command should be skipped")
	}
}
