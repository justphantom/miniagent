package config

import (
	"testing"
	"time"

	"github.com/justphantom/miniagent/miniagent"
)

func iptr(v int) *int       { return &v }
func sptr(s string) *string { return &s }

// layeredCfg builds a config with model parameters set at all three layers: run(global) < provider < model(m1);
// m2 has no model-level config (falls back to provider-level); thinking.map contains off/fast/medium/slow.
func layeredCfg() *Config {
	return &Config{
		Providers: []ProviderConfig{{
			Name:          "p",
			ChatURL:       "https://a/v1/chat/completions",
			Thinking:      &miniagent.ThinkingMapping{Field: "x-effort", Map: map[string]string{"off": "off", "fast": "low", "medium": "mid", "slow": "high"}},
			ThinkingLevel: sptr("medium"),
			MaxTokens:     iptr(2000),
			ContextWindow: iptr(100000),
			HTTPTimeout:   sptr("100s"),
			Models: []ModelConfig{
				{Name: "m1", MaxTokens: iptr(3000), ContextWindow: iptr(200000), Thinking: sptr("fast")},
				{Name: "m2"},
			},
		}},
		Defaults:   DefaultsConfig{Provider: "p", Model: "m1", Thinking: "slow"},
		Compaction: CompactionConfig{Provider: "p", Model: "m1"},
		Run:        RunConfig{MaxTokens: iptr(1000), ContextWindow: iptr(50000), HTTPTimeout: sptr("200s")},
	}
}

// max_tokens/context_window: model > provider > global; an undeclared model also falls back to provider-level.
func TestResolve_ModelParamsLayered(t *testing.T) {
	cfg := layeredCfg()
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatalf("resolve m1: %v", err)
	}
	if r.MaxTokens == nil || *r.MaxTokens != 3000 {
		t.Errorf("m1 MaxTokens=%v want 3000 (model)", r.MaxTokens)
	}
	if r.ContextWindow == nil || *r.ContextWindow != 200000 {
		t.Errorf("m1 ContextWindow=%v want 200000 (model)", r.ContextWindow)
	}

	cfg.Defaults.Model = "m2" // no model-level → provider-level
	if r2, err := Resolve(cfg, CLIOverrides{}); err != nil || r2.MaxTokens == nil || *r2.MaxTokens != 2000 {
		t.Errorf("m2 MaxTokens=%v want 2000 (provider), err=%v", r2.MaxTokens, err)
	}

	cfg.Defaults.Model = "m3" // provider does not declare this model → still falls back to provider-level
	if r3, err := Resolve(cfg, CLIOverrides{}); err != nil || r3.MaxTokens == nil || *r3.MaxTokens != 2000 {
		t.Errorf("m3 MaxTokens=%v want 2000 (provider, undeclared model), err=%v", r3.MaxTokens, err)
	}
}

// Neither provider nor model declares → global run.
func TestResolve_ModelParamsFallthroughGlobal(t *testing.T) {
	cfg := &Config{
		Providers:  []ProviderConfig{{Name: "p", ChatURL: "https://a/v1/chat/completions", Models: []ModelConfig{{Name: "m"}}}},
		Defaults:   DefaultsConfig{Provider: "p", Model: "m"},
		Compaction: CompactionConfig{Provider: "p", Model: "m"},
		Run:        RunConfig{MaxTokens: iptr(1000), ContextWindow: iptr(50000)},
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxTokens == nil || *r.MaxTokens != 1000 {
		t.Errorf("MaxTokens=%v want 1000 (global)", r.MaxTokens)
	}
	if r.ContextWindow == nil || *r.ContextWindow != 50000 {
		t.Errorf("ContextWindow=%v want 50000 (global)", r.ContextWindow)
	}
}

// http_timeout is provider > global only (no model layer).
func TestResolve_HTTPTimeoutProviderOverGlobal(t *testing.T) {
	cfg := layeredCfg()
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTPTimeout == nil || *r.HTTPTimeout != 100*time.Second {
		t.Errorf("HTTPTimeout=%v want 100s (provider)", r.HTTPTimeout)
	}
	cfg.Providers[0].HTTPTimeout = nil // provider unset → global
	if r2, _ := Resolve(cfg, CLIOverrides{}); r2.HTTPTimeout == nil || *r2.HTTPTimeout != 200*time.Second {
		t.Errorf("HTTPTimeout=%v want 200s (global)", r2.HTTPTimeout)
	}
}

// thinking level: cli > model > provider > defaults.
func TestResolve_ThinkingLayered(t *testing.T) {
	cfg := layeredCfg()
	if r, _ := Resolve(cfg, CLIOverrides{}); r.Thinking != "fast" {
		t.Errorf("m1 Thinking=%q want fast (model)", r.Thinking)
	}
	cfg.Defaults.Model = "m2" // no model-level → provider(medium)
	if r, _ := Resolve(cfg, CLIOverrides{}); r.Thinking != "medium" {
		t.Errorf("m2 Thinking=%q want medium (provider)", r.Thinking)
	}
	cfg.Providers[0].ThinkingLevel = nil // provider has no level → defaults(slow)
	if r, _ := Resolve(cfg, CLIOverrides{}); r.Thinking != "slow" {
		t.Errorf("m2 Thinking=%q want slow (defaults)", r.Thinking)
	}
	cli := "slow" // cli overrides everything
	cfg.Defaults.Model = "m1"
	if r, _ := Resolve(cfg, CLIOverrides{Thinking: &cli}); r.Thinking != "slow" {
		t.Errorf("cli Thinking=%q want slow (cli)", r.Thinking)
	}
}

// Model-level/provider-level thinking level must go through provider.thinking.map; an invalid level errors at Resolve.
func TestResolve_ThinkingLevelScoped(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:          "p",
			ChatURL:       "https://a/v1/chat/completions",
			Thinking:      &miniagent.ThinkingMapping{Field: "x-effort", Map: map[string]string{"fast": "low"}},
			ThinkingLevel: sptr("bogus"), // not in map
			Models:        []ModelConfig{{Name: "m"}},
		}},
		Defaults:   DefaultsConfig{Provider: "p", Model: "m"},
		Compaction: CompactionConfig{Provider: "p", Model: "m"},
	}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("invalid provider thinking_level (not in map) should error at Resolve")
	}
}
