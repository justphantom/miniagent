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

// loadProjectRules：workdir 有 .miniagent 时优先 workdir；无 workdir 时读 home 目录。
// home 不存在时返回零值（不报错）。
func TestLoadProjectRules_WorkdirOverridesHome(t *testing.T) {
	homeDir := t.TempDir()
	workdir := t.TempDir()
	// home 目录有 rules
	writeFile(t, filepath.Join(homeDir, ".miniagent", "rules.md"), "home-rule")
	writeFile(t, filepath.Join(homeDir, ".miniagent", "persona.md"), "home-persona")
	// workdir 有 rules（覆盖 home）
	writeFile(t, filepath.Join(workdir, ".miniagent", "rules.md"), "workdir-rule")
	writeFile(t, filepath.Join(workdir, ".miniagent", "persona.md"), "workdir-persona")

	// 临时切换 HOME 使 homeDir 成为 "home"
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", homeDir)
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules(workdir)
	if pr.rules != "workdir-rule" {
		t.Errorf("rules should be from workdir: got %q", pr.rules)
	}
	if pr.persona != "workdir-persona" {
		t.Errorf("persona should be from workdir: got %q", pr.persona)
	}
}

func TestLoadProjectRules_UsesHomeWhenNoWorkdir(t *testing.T) {
	homeDir := t.TempDir()
	writeFile(t, filepath.Join(homeDir, ".miniagent", "rules.md"), "home-rule")
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", homeDir)
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules("")
	if pr.rules != "home-rule" {
		t.Errorf("rules should be from home when workdir empty: got %q", pr.rules)
	}
}

func TestLoadProjectRules_EmptyWorkdirNoHome(t *testing.T) {
	homeDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", homeDir)
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules("/nonexistent")
	if pr.hasAny() {
		t.Error("should have no rules when workdir doesn't exist")
	}
}

// loadProjectRules：workdir 有完整 .miniagent 设置时正确加载所有来源。
func TestLoadProjectRules_AllWorkdirSources(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".miniagent", "persona.md"), "你是本项目专属 agent。")
	writeFile(t, filepath.Join(workdir, ".miniagent", "rules.md"), "禁止提交未跑测试的代码。")
	writeFile(t, filepath.Join(workdir, ".miniagent", "scripts.json"), `{"scripts":[{"name":"test","command":"go test ./...","description":"跑测试"}]}`)

	// 隔离 home，避免真实 ~/.miniagent 干扰。
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
	if len(pr.scripts) != 1 || pr.scripts[0].Name != "test" || pr.scripts[0].Command != "go test ./..." {
		t.Errorf("scripts = %+v", pr.scripts)
	}
	if !pr.hasAny() {
		t.Error("hasAny should be true")
	}
}

// mergeProjectRules：workdir 存在即覆盖 home，否则 home 保留。
func TestMergeProjectRules_WorkdirWins(t *testing.T) {
	home := projectRules{persona: "h-p", rules: "h-r"}
	workdir := projectRules{persona: "w-p", personaSet: true, rules: "w-r", rulesSet: true}
	merged := mergeProjectRules(workdir, home)
	if merged.persona != "w-p" || merged.rules != "w-r" {
		t.Errorf("workdir should win: got %+v", merged)
	}
}

func TestMergeProjectRules_HomeKeptWhenWorkdirEmpty(t *testing.T) {
	home := projectRules{persona: "h-p", rules: "h-r"}
	workdir := projectRules{}
	merged := mergeProjectRules(workdir, home)
	if merged.persona != "h-p" || merged.rules != "h-r" {
		t.Errorf("home should be kept: got %+v", merged)
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

// buildTools 注册 script_<name> 工具，跳过 name/command 为空者。
func TestBuildTools_RegistersScriptTools(t *testing.T) {
	scripts := []scriptDef{
		{"test", "go test ./...", "跑测试"},
		{"", "echo x", "空 name 跳过"},
		{"lint", "", "空 command 跳过"},
	}
	tools := buildTools(t.TempDir(), 0, 0, 0, miniagent.ModeAuto, 0, scripts)
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
