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

// CLI 传入非法 mode 必须报错，而非被静默当作 auto。
func TestResolve_InvalidCliModeErrors(t *testing.T) {
	cfg, _ := LoadConfig(writeTmpConfig(t, validConfigBody()))
	badMode := "invalid_mode"
	if _, err := Resolve(cfg, CLIOverrides{Mode: &badMode}); err == nil {
		t.Error("invalid CLI mode should error, not silently become auto")
	}
}

// config defaults.mode 已通过 validateConfig 校验，Resolve 对 CLI 覆盖做二次枚举校验。
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

// S1 删裸模式：Resolve(nil, ...) 必须报错（cfg 必须非 nil）。
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
	// 配置中 duration 字符串非法（如缺单位）应上抛错误，而非静默回落。
	bad := "30"
	cfg := mkFullConfig("p", "m")
	cfg.Run = RunConfig{MaxDuration: &bad}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("bad duration string should error, not silently drop")
	}
}

// S4：5 个策略化常量经 config run.* 解析后透传到 ResolvedRun（仅 config 来源，不经 CLI）。
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

// 新字段：summary_request 和 summarizer_prompt 在 config 中正确解析。
func TestResolve_PromptFields(t *testing.T) {
	cfg := mkFullConfig("p", "m")
	cfg.Defaults.SummaryRequest = "自定义总结引导"
	cfg.Defaults.SummarizerPrompt = "自定义压缩器"
	r, err := Resolve(cfg, CLIOverrides{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.SummaryRequest != "自定义总结引导" {
		t.Errorf("SummaryRequest = %q, want 自定义总结引导", r.SummaryRequest)
	}
	if r.SummarizerPrompt != "自定义压缩器" {
		t.Errorf("SummarizerPrompt = %q, want 自定义压缩器", r.SummarizerPrompt)
	}
}

// v3.2.3 新增的 4 个策略化常量曾漏装配进 ResolvedRun（resolveRun 未赋值），config 值静默失效；
// 修复后须从 config 透传到 ResolvedRun，main 的 Set* 才能据此覆盖内置默认。
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
