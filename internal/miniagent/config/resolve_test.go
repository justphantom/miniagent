package config

import (
	"testing"
	"time"
)

func TestResolve_CLIOverridesConfig(t *testing.T) {
	cfg, err := LoadConfig(writeTmpConfig(t, validConfigBody()))
	if err != nil {
		t.Fatal(err)
	}
	cliProvider := "main"
	cliModel := "glm-5.2"
	r, err := Resolve(cfg, CLIOverrides{Provider: &cliProvider, Model: &cliModel})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.ModelID != "glm-5.2" {
		t.Errorf("ModelID = %q", r.ModelID)
	}
	if r.Mode != "default" {
		t.Errorf("Mode = %q want default", r.Mode)
	}
}

func TestResolve_DefaultsModel(t *testing.T) {
	cfg, _ := LoadConfig(writeTmpConfig(t, validConfigBody()))
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.ModelID != "glm" {
		t.Errorf("ModelID = %q want glm (from defaults)", r.ModelID)
	}
}

// An invalid CLI mode must error, not be silently treated as auto.
func TestResolve_InvalidCliModeErrors(t *testing.T) {
	cfg, _ := LoadConfig(writeTmpConfig(t, validConfigBody()))
	badMode := "invalid_mode"
	if _, err := Resolve(cfg, CLIOverrides{Mode: &badMode}); err == nil {
		t.Error("invalid CLI mode should error, not silently become auto")
	}
}

// config defaults.mode has already been validated by validateConfig; Resolve re-validates the CLI override against the enum.
func TestResolve_AutoModeAllowed(t *testing.T) {
	cfg, _ := LoadConfig(writeTmpConfig(t, validConfigBody()))
	autoMode := "auto"
	r, err := Resolve(cfg, CLIOverrides{Mode: &autoMode})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Mode != "auto" {
		t.Errorf("Mode = %q want auto", r.Mode)
	}
}

// S1 removed bare mode: Resolve(nil, ...) must error (cfg must be non-nil).
func TestResolve_NilCfgErrors(t *testing.T) {
	model := "glm"
	if _, err := Resolve(nil, CLIOverrides{Model: &model}); err == nil {
		t.Error("Resolve with nil cfg should error after S1")
	}
}

func TestResolve_DurationFromString(t *testing.T) {
	dur := "30s"
	cfg := mkFullConfig("p", "m")
	cfg.Run = RunConfig{MaxDuration: &dur}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Run.MaxDuration == nil || *r.Run.MaxDuration != 30*time.Second {
		t.Errorf("MaxDuration = %v", r.Run.MaxDuration)
	}
}

func TestResolve_BadDurationFromString(t *testing.T) {
	// An invalid duration string in config (e.g. missing unit) should surface an error, not silently fall back.
	bad := "30"
	cfg := mkFullConfig("p", "m")
	cfg.Run = RunConfig{MaxDuration: &bad}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("bad duration string should error, not silently drop")
	}
}

// S4: 5 strategized constants are parsed from config run.* and passed through to ResolvedRun (config-only source, not via CLI).
func TestResolve_StrategyConstants(t *testing.T) {
	mk := func(v int) *int { return &v }
	cfg := mkFullConfig("p", "m")
	cfg.Run = RunConfig{
		MaxToolResultChars:   mk(1234),
		MaxFileResultChars:   mk(9999),
		MaxParallelTools:     mk(3),
		ContextKeepRecent:    mk(8),
		SummaryMaxChars:      mk(1500),
		ContextKeepReasoning: mk(2),
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  *int
		want int
	}{
		{"MaxToolResultChars", r.RunConfig.MaxToolResultChars, 1234},
		{"MaxFileResultChars", r.RunConfig.MaxFileResultChars, 9999},
		{"MaxParallelTools", r.RunConfig.MaxParallelTools, 3},
		{"ContextKeepRecent", r.RunConfig.ContextKeepRecent, 8},
		{"SummaryMaxChars", r.RunConfig.SummaryMaxChars, 1500},
		{"ContextKeepReasoning", r.RunConfig.ContextKeepReasoning, 2},
	}
	for _, c := range checks {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}

// New fields: summary_request and summarizer_prompt are correctly parsed from config.
func TestResolve_PromptFields(t *testing.T) {
	cfg := mkFullConfig("p", "m")
	cfg.Defaults.SummaryRequest = "custom summary guidance"
	cfg.Defaults.SummarizerPrompt = "custom compactor"
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.SummaryRequest != "custom summary guidance" {
		t.Errorf("SummaryRequest = %q, want custom summary guidance", r.SummaryRequest)
	}
	if r.SummarizerPrompt != "custom compactor" {
		t.Errorf("SummarizerPrompt = %q, want custom compactor", r.SummarizerPrompt)
	}
}

// The 4 strategized constants added in v3.2.3 were once missed when wiring into ResolvedRun (resolveRun did not assign them),
// so config values silently had no effect; after the fix they must pass through from config to ResolvedRun so main's Set* can override the built-in defaults.
func TestResolve_StrategyConstantsLateWired(t *testing.T) {
	mk := func(v int) *int { return &v }
	cfg := mkFullConfig("p", "m")
	cfg.Run = RunConfig{
		SummaryMaxTokens:     mk(512),
		GrepMaxMatches:       mk(500),
		ContextTrimToolChars: mk(1234),
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  *int
		want int
	}{
		{"SummaryMaxTokens", r.RunConfig.SummaryMaxTokens, 512},
		{"GrepMaxMatches", r.RunConfig.GrepMaxMatches, 500},
		{"ContextTrimToolChars", r.RunConfig.ContextTrimToolChars, 1234},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}
