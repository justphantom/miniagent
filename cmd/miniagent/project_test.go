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

// loadProjectRules: reads persona/rules when workdir has .miniagent.
func TestLoadProjectRules_WorkdirOnly(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".miniagent", "rules.md"), "workdir-rule")
	writeFile(t, filepath.Join(workdir, ".miniagent", "persona.md"), "workdir-persona")

	// Isolate home to confirm it only reads workdir (the global layer was removed).
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

// loadProjectRules: no rules when workdir does not exist.
func TestLoadProjectRules_EmptyWorkdirNoRules(t *testing.T) {
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules("/nonexistent")
	if pr.hasAny() {
		t.Error("should have no rules when workdir doesn't exist")
	}
}

// loadProjectRules: correctly loads all sources when workdir has a complete .miniagent.
func TestLoadProjectRules_AllSources(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".miniagent", "persona.md"), "You are the dedicated agent for this project.")
	writeFile(t, filepath.Join(workdir, ".miniagent", "rules.md"), "Do not commit code without running tests.")

	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	pr := loadProjectRules(workdir)
	if pr.persona != "You are the dedicated agent for this project." {
		t.Errorf("persona = %q", pr.persona)
	}
	if !strings.Contains(pr.rules, "Do not commit") {
		t.Errorf("rules = %q", pr.rules)
	}
	if !pr.hasAny() {
		t.Error("hasAny should be true")
	}
}

// Global ~/.miniagent/ is no longer read (the global layer was removed).
func TestLoadProjectRules_HomeIgnored(t *testing.T) {
	homeDir := t.TempDir()
	writeFile(t, filepath.Join(homeDir, ".miniagent", "rules.md"), "home-rule")
	writeFile(t, filepath.Join(homeDir, ".miniagent", "persona.md"), "home-persona")
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", homeDir)
	defer os.Setenv("HOME", oldHome)

	// No workdir → does not read home → no rules.
	if pr := loadProjectRules(""); pr.hasAny() {
		t.Errorf("global ~/.miniagent/ should no longer be read, got %+v", pr)
	}
}

// mergeSystemPrompt: persona replaces base; rules are appended; when hasAny the LLM is informed.
func TestMergeSystemPrompt_PersonaOverridesBase(t *testing.T) {
	got := mergeSystemPrompt("default-workflow", "dedicated-persona", "ruleA", true)
	if !strings.HasPrefix(got, "dedicated-persona") {
		t.Errorf("persona should be base: %s", got)
	}
	if !strings.Contains(got, "## Project Rules\nruleA") {
		t.Errorf("rules missing: %s", got)
	}
	if !strings.Contains(got, ".miniagent/ project rules loaded") {
		t.Errorf("should note loaded rules: %s", got)
	}
}

// mergeSystemPrompt: keeps base (workflow constraints) when there is no persona, only appends rules.
func TestMergeSystemPrompt_KeepsBaseWhenNoPersona(t *testing.T) {
	got := mergeSystemPrompt("default-workflow", "", "ruleA", true)
	if !strings.HasPrefix(got, "default-workflow") {
		t.Errorf("base should be kept: %s", got)
	}
	if !strings.Contains(got, "ruleA") {
		t.Errorf("rules missing: %s", got)
	}
}
