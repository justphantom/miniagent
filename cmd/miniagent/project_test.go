package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// loadProjectRules：workdir 有 .miniagent 时读取 persona/rules。
func TestLoadProjectRules_WorkdirOnly(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".miniagent", "rules.md"), "workdir-rule")
	writeFile(t, filepath.Join(workdir, ".miniagent", "persona.md"), "workdir-persona")

	// 隔离 home，确认仅读 workdir（全局层已取消）。
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules(workdir)
	if pr.rules != "workdir-rule" {
		t.Errorf("rules = %q want workdir-rule", pr.rules)
	}
	if pr.persona != "workdir-persona" {
		t.Errorf("persona = %q want workdir-persona", pr.persona)
	}
}

// loadProjectRules：workdir 不存在时无规则。
func TestLoadProjectRules_EmptyWorkdirNoRules(t *testing.T) {
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules("/nonexistent")
	if pr.hasAny() {
		t.Error("should have no rules when workdir doesn't exist")
	}
}

// loadProjectRules：workdir 有完整 .miniagent 时正确加载所有来源。
func TestLoadProjectRules_AllSources(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".miniagent", "persona.md"), "你是本项目专属 agent。")
	writeFile(t, filepath.Join(workdir, ".miniagent", "rules.md"), "禁止提交未跑测试的代码。")

	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules(workdir)
	if pr.persona != "你是本项目专属 agent。" {
		t.Errorf("persona = %q", pr.persona)
	}
	if !strings.Contains(pr.rules, "禁止提交") {
		t.Errorf("rules = %q", pr.rules)
	}
	if !pr.hasAny() {
		t.Error("hasAny should be true")
	}
}

// 全局 ~/.miniagent/ 不再读取（全局层已取消）。
func TestLoadProjectRules_HomeIgnored(t *testing.T) {
	homeDir := t.TempDir()
	writeFile(t, filepath.Join(homeDir, ".miniagent", "rules.md"), "home-rule")
	writeFile(t, filepath.Join(homeDir, ".miniagent", "persona.md"), "home-persona")
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", homeDir)
	defer os.Setenv("HOME", oldHome)

	// 无 workdir → 不读 home → 无规则。
	if pr := loadProjectRules(""); pr.hasAny() {
		t.Errorf("全局 ~/.miniagent/ 不应再读取，got %+v", pr)
	}
}

// mergeSystemPrompt：persona 取代 base；rules 追加；hasAny 时告知 LLM。
func TestMergeSystemPrompt_PersonaOverridesBase(t *testing.T) {
	got := mergeSystemPrompt("default-workflow", "专属人格", "规则A", true)
	if !strings.HasPrefix(got, "专属人格") {
		t.Errorf("persona should be base: %s", got)
	}
	if !strings.Contains(got, "## 项目规则\n规则A") {
		t.Errorf("rules missing: %s", got)
	}
	if !strings.Contains(got, "已加载 .miniagent/") {
		t.Errorf("should note loaded rules: %s", got)
	}
}

// mergeSystemPrompt：无 persona 时沿用 base（工作流约束），仅追加 rules。
func TestMergeSystemPrompt_KeepsBaseWhenNoPersona(t *testing.T) {
	got := mergeSystemPrompt("default-workflow", "", "规则A", true)
	if !strings.HasPrefix(got, "default-workflow") {
		t.Errorf("base should be kept: %s", got)
	}
	if !strings.Contains(got, "规则A") {
		t.Errorf("rules missing: %s", got)
	}
}
