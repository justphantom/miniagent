package miniagent

import (
	"testing"
)

// compaction/memory 成对设置时取自身配置（可跨 provider）。
func TestResolve_SecondaryIndependentPairs(t *testing.T) {
	cfg := mkFullConfig("main", "glm",
		ProviderConfig{Name: "main", ChatURL: "https://a/v1/chat/completions"},
		ProviderConfig{Name: "comp", ChatURL: "https://c/v1/chat/completions"},
		ProviderConfig{Name: "mem", ChatURL: "https://m/v1/chat/completions"},
	)
	cfg.Compaction = CompactionConfig{Provider: "comp", Model: "glm-flash"}
	cfg.Memory = MemoryConfig{Provider: "mem", Model: "glm-mini"}
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CompactionProvider.Name != "comp" || r.CompactionModelID != "glm-flash" {
		t.Errorf("compaction = %s/%s, want comp/glm-flash", r.CompactionProvider.Name, r.CompactionModelID)
	}
	if r.MemoryProvider.Name != "mem" || r.MemoryModelID != "glm-mini" {
		t.Errorf("memory = %s/%s, want mem/glm-mini", r.MemoryProvider.Name, r.MemoryModelID)
	}
}

// compaction/memory 整段留空时整体回落 defaults 对；即使 CLI 覆盖了主会话，回落目标仍是
// config defaults（成对规则：不允许 CLI 主会话与二级交叉）。
func TestResolve_SecondaryFallsBackToDefaultsPair(t *testing.T) {
	cfg := mkFullConfig("main", "glm",
		ProviderConfig{Name: "main", ChatURL: "https://a/v1/chat/completions"},
		ProviderConfig{Name: "other", ChatURL: "https://b/v1/chat/completions"},
	)
	cfg.Compaction = CompactionConfig{}
	cfg.Memory = MemoryConfig{}
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
		t.Errorf("compaction = %s/%s, want 回落 defaults 对 main/glm", r.CompactionProvider.Name, r.CompactionModelID)
	}
	if r.MemoryProvider.Name != "main" || r.MemoryModelID != "glm" {
		t.Errorf("memory = %s/%s, want 回落 defaults 对 main/glm", r.MemoryProvider.Name, r.MemoryModelID)
	}
}

// compaction.provider 指向未声明 provider 时报错（Resolve 对手工构造 Config 二次校验）。
func TestResolve_CompactionUnknownProviderErrors(t *testing.T) {
	cfg := mkFullConfig("main", "glm")
	cfg.Compaction = CompactionConfig{Provider: "unknown", Model: "glm-flash"}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("compaction.provider unknown should error")
	}
}

func TestResolve_MemoryUnknownProviderErrors(t *testing.T) {
	cfg := mkFullConfig("main", "glm")
	cfg.Memory = MemoryConfig{Provider: "unknown", Model: "glm-mini"}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("memory.provider unknown should error")
	}
}

// 成对规则：compaction/memory 只设 provider 或只设 model 均报错（同空才回落 defaults）。
func TestResolve_SecondaryHalfSetErrors(t *testing.T) {
	cases := map[string]func(*Config){
		"compaction 仅 provider": func(c *Config) { c.Compaction = CompactionConfig{Provider: "main"} },
		"compaction 仅 model":    func(c *Config) { c.Compaction = CompactionConfig{Model: "m"} },
		"memory 仅 provider":     func(c *Config) { c.Memory = MemoryConfig{Provider: "main"} },
		"memory 仅 model":        func(c *Config) { c.Memory = MemoryConfig{Model: "m"} },
	}
	for name, mutate := range cases {
		cfg := mkFullConfig("main", "glm")
		mutate(cfg)
		if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
			t.Errorf("%s: should error", name)
		}
	}
}

// 成对规则：CLI -provider/-model 只传其一即报错。
func TestResolve_CliPairRequired(t *testing.T) {
	p, m := "main", "glm-pro"
	if _, err := Resolve(mkFullConfig("main", "glm"), CLIOverrides{Provider: &p}); err == nil {
		t.Error("only -provider should error")
	}
	if _, err := Resolve(mkFullConfig("main", "glm"), CLIOverrides{Model: &m}); err == nil {
		t.Error("only -model should error")
	}
}

// CLI 成对覆盖主会话；二级仍取 config（自身对或 defaults 对），不受 CLI 影响。
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

// CLI 对中 provider 未声明时报错。
func TestResolve_CliUnknownProviderErrors(t *testing.T) {
	bad, m := "nope", "glm"
	if _, err := Resolve(mkFullConfig("main", "glm"), CLIOverrides{Provider: &bad, Model: &m}); err == nil {
		t.Error("CLI -provider unknown should error")
	}
}
