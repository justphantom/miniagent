package miniagent

import (
	"testing"
)

func TestResolve_CompactionFallback(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "main", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:  DefaultsConfig{Model: "main/glm"},
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CompactionProvider.Name != "main" {
		t.Errorf("CompactionProvider.Name = %q, want main", r.CompactionProvider.Name)
	}
	if r.CompactionModelID != "glm" {
		t.Errorf("CompactionModelID = %q, want glm", r.CompactionModelID)
	}
}

// compaction.model 可指定跨 provider 的 model。
func TestResolve_CompactionCrossProvider(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "main", ChatURL: "https://a/v1/chat/completions"},
			{Name: "comp", ChatURL: "https://c/v1/chat/completions"},
		},
		Defaults:   DefaultsConfig{Model: "main/glm"},
		Compaction: CompactionConfig{Model: "comp/glm-flash"},
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CompactionProvider.Name != "comp" {
		t.Errorf("CompactionProvider.Name = %q, want comp", r.CompactionProvider.Name)
	}
	if r.CompactionModelID != "glm-flash" {
		t.Errorf("CompactionModelID = %q, want glm-flash", r.CompactionModelID)
	}
}

// compaction.model 带 / 但 provider 不存在时报错。
func TestResolve_CompactionUnknownProviderErrors(t *testing.T) {
	cfg := &Config{
		Providers:  []ProviderConfig{{Name: "main", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:   DefaultsConfig{Model: "main/glm"},
		Compaction: CompactionConfig{Model: "unknown/glm-flash"},
	}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("compaction.model with unknown provider should error")
	}
}

// 三级回落：CLI -model 覆盖 defaults.model 时，compaction/memory 应取 defaults.model 而非 CLI 主模型。
func TestResolve_SecondaryFallsBackToDefaultsNotCliModel(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "main", ChatURL: "https://a/v1/chat/completions"},
			{Name: "other", ChatURL: "https://b/v1/chat/completions"},
		},
		Defaults: DefaultsConfig{Model: "main/glm-flash"},
	}
	cliModel := "other/glm-pro"
	r, err := Resolve(cfg, CLIOverrides{Model: &cliModel})
	if err != nil {
		t.Fatal(err)
	}
	// 主会话 = CLI 覆盖 other/glm-pro。
	if r.Provider.Name != "other" || r.ModelID != "glm-pro" {
		t.Errorf("main = %s/%s, want other/glm-pro", r.Provider.Name, r.ModelID)
	}
	// compaction / memory 空 → defaults.model = main/glm-flash（不跟 CLI 覆盖）。
	if r.CompactionProvider.Name != "main" || r.CompactionModelID != "glm-flash" {
		t.Errorf("compaction = %s/%s, want main/glm-flash", r.CompactionProvider.Name, r.CompactionModelID)
	}
	if r.MemoryProvider.Name != "main" || r.MemoryModelID != "glm-flash" {
		t.Errorf("memory = %s/%s, want main/glm-flash", r.MemoryProvider.Name, r.MemoryModelID)
	}
}

// 三级回落兜底：memory.model 与 defaults.model 均空时，回落主会话模型。
func TestResolve_SecondaryFallsBackToMainWhenNoDefaults(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "main", ChatURL: "https://a/v1/chat/completions"},
			{Name: "other", ChatURL: "https://b/v1/chat/completions"},
		},
	}
	cliModel := "other/glm-pro"
	r, err := Resolve(cfg, CLIOverrides{Model: &cliModel})
	if err != nil {
		t.Fatal(err)
	}
	if r.CompactionProvider.Name != "other" || r.CompactionModelID != "glm-pro" {
		t.Errorf("compaction = %s/%s, want other/glm-pro", r.CompactionProvider.Name, r.CompactionModelID)
	}
	if r.MemoryProvider.Name != "other" || r.MemoryModelID != "glm-pro" {
		t.Errorf("memory = %s/%s, want other/glm-pro", r.MemoryProvider.Name, r.MemoryModelID)
	}
}

// memory.model 显式跨 provider；compaction 仍走 defaults.model。
func TestResolve_MemoryModelExplicit(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "main", ChatURL: "https://a/v1/chat/completions"},
			{Name: "comp", ChatURL: "https://c/v1/chat/completions"},
		},
		Defaults:   DefaultsConfig{Model: "main/glm"},
		Compaction: CompactionConfig{},
		Memory:     MemoryConfig{Model: "comp/glm-mini"},
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CompactionProvider.Name != "main" || r.CompactionModelID != "glm" {
		t.Errorf("compaction = %s/%s, want main/glm", r.CompactionProvider.Name, r.CompactionModelID)
	}
	if r.MemoryProvider.Name != "comp" || r.MemoryModelID != "glm-mini" {
		t.Errorf("memory = %s/%s, want comp/glm-mini", r.MemoryProvider.Name, r.MemoryModelID)
	}
}

// memory.model 不带 '/' → 与主会话同 provider，只换 model id。
func TestResolve_MemoryModelSameProviderNoSlash(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "main", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:  DefaultsConfig{Model: "main/glm"},
		Memory:    MemoryConfig{Model: "glm-mini"},
	}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.MemoryProvider.Name != "main" || r.MemoryModelID != "glm-mini" {
		t.Errorf("memory = %s/%s, want main/glm-mini", r.MemoryProvider.Name, r.MemoryModelID)
	}
}
