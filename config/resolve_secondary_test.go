package config

import (
	"testing"
)

// When compaction is set as a pair it takes its own config (may cross providers).
func TestResolve_SecondaryIndependentPairs(t *testing.T) {
	cfg := mkFullConfig("main", "glm",
		ProviderConfig{Name: "main", ChatURL: "https://a/v1/chat/completions"},
		ProviderConfig{Name: "comp", ChatURL: "https://c/v1/chat/completions"},
	)
	cfg.Compaction = CompactionConfig{Provider: "comp", Model: "glm-flash"}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CompactionProvider.Name != "comp" || r.CompactionModelID != "glm-flash" {
		t.Errorf("compaction = %s/%s, want comp/glm-flash", r.CompactionProvider.Name, r.CompactionModelID)
	}
}

// When the compaction section is entirely empty it falls back to the defaults pair; even if CLI overrides the main session,
// the fallback target is still the config defaults (pair rule: no cross between the CLI main session and the secondary).
func TestResolve_SecondaryFallsBackToDefaultsPair(t *testing.T) {
	cfg := mkFullConfig("main", "glm",
		ProviderConfig{Name: "main", ChatURL: "https://a/v1/chat/completions"},
		ProviderConfig{Name: "other", ChatURL: "https://b/v1/chat/completions"},
	)
	cfg.Compaction = CompactionConfig{}
	cliProvider := "other"
	cliModel := "glm-pro"
	r, err := Resolve(cfg, CLIOverrides{Provider: &cliProvider, Model: &cliModel})
	if err != nil {
		t.Fatal(err)
	}
	if r.Provider.Name != "other" || r.ModelID != "glm-pro" {
		t.Errorf("main = %s/%s, want other/glm-pro", r.Provider.Name, r.ModelID)
	}
	if r.CompactionProvider.Name != "main" || r.CompactionModelID != "glm" {
		t.Errorf("compaction = %s/%s, want fallback defaults pair main/glm", r.CompactionProvider.Name, r.CompactionModelID)
	}
}

// When compaction.provider points to an undeclared provider it errors (Resolve re-validates a hand-constructed Config).
func TestResolve_CompactionUnknownProviderErrors(t *testing.T) {
	cfg := mkFullConfig("main", "glm")
	cfg.Compaction = CompactionConfig{Provider: "unknown", Model: "glm-flash"}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("compaction.provider unknown should error")
	}
}

// Pair rule: setting only provider or only model for compaction errors (only both-empty falls back to defaults).
func TestResolve_SecondaryHalfSetErrors(t *testing.T) {
	cases := map[string]func(*Config){
		"compaction provider only": func(c *Config) { c.Compaction = CompactionConfig{Provider: "main"} },
		"compaction model only":    func(c *Config) { c.Compaction = CompactionConfig{Model: "m"} },
	}
	for name, mutate := range cases {
		cfg := mkFullConfig("main", "glm")
		mutate(cfg)
		if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
			t.Errorf("%s: should error", name)
		}
	}
}

// Pair rule: passing only one of CLI -provider/-model errors.
func TestResolve_CliPairRequired(t *testing.T) {
	p, m := "main", "glm-pro"
	if _, err := Resolve(mkFullConfig("main", "glm"), CLIOverrides{Provider: &p}); err == nil {
		t.Error("only -provider should error")
	}
	if _, err := Resolve(mkFullConfig("main", "glm"), CLIOverrides{Model: &m}); err == nil {
		t.Error("only -model should error")
	}
}

// CLI overrides the main session as a pair; the secondary still takes config (its own pair or the defaults pair), unaffected by CLI.
func TestResolve_CliPairOverride(t *testing.T) {
	cfg := mkFullConfig("main", "glm",
		ProviderConfig{Name: "main", ChatURL: "https://a/v1/chat/completions"},
		ProviderConfig{Name: "other", ChatURL: "https://b/v1/chat/completions"},
	)
	cliProvider := "other"
	cliModel := "glm-pro"
	r, err := Resolve(cfg, CLIOverrides{Provider: &cliProvider, Model: &cliModel})
	if err != nil {
		t.Fatal(err)
	}
	if r.Provider.Name != "other" || r.ModelID != "glm-pro" {
		t.Errorf("main = %s/%s, want other/glm-pro", r.Provider.Name, r.ModelID)
	}
	if r.CompactionProvider.Name != "main" || r.CompactionModelID != "glm" {
		t.Errorf("compaction = %s/%s, want main/glm", r.CompactionProvider.Name, r.CompactionModelID)
	}
}

// When the provider in the CLI pair is undeclared it errors.
func TestResolve_CliUnknownProviderErrors(t *testing.T) {
	bad, m := "nope", "glm"
	if _, err := Resolve(mkFullConfig("main", "glm"), CLIOverrides{Provider: &bad, Model: &m}); err == nil {
		t.Error("CLI -provider unknown should error")
	}
}
