package miniagent

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestResolve_CLIOverridesConfig(t *testing.T) {
	cfg, err := LoadConfig(writeTmpConfig(t, validConfigBody()))
	if err != nil {
		t.Fatal(err)
	}
	cliModel := "main/glm-5.2"
	r, err := Resolve(cfg, CLIOverrides{Model: &cliModel})
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
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:  DefaultsConfig{Model: "p/m"},
		Run:       RunConfig{MaxDuration: &dur},
	}
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
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:  DefaultsConfig{Model: "p/m"},
		Run:       RunConfig{MaxDuration: &bad},
	}
	if _, err := Resolve(cfg, CLIOverrides{}); err == nil {
		t.Error("bad duration string should error, not silently drop")
	}
}

// S4：5 个策略化常量经 config run.* 解析后透传到 ResolvedRun（仅 config 来源，不经 CLI）。
func TestResolve_StrategyConstants(t *testing.T) {
	mk := func(v int) *int { return &v }
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:  DefaultsConfig{Model: "p/m"},
		Run: RunConfig{
			MaxToolResultChars: mk(1234),
			MaxFileResultChars: mk(9999),
			MaxParallelTools:   mk(3),
			ContextKeepRecent:  mk(8),
			SummaryMaxChars:    mk(1500),
		},
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
		{"MaxToolResultChars", r.Run.MaxToolResultChars, 1234},
		{"MaxFileResultChars", r.Run.MaxFileResultChars, 9999},
		{"MaxParallelTools", r.Run.MaxParallelTools, 3},
		{"ContextKeepRecent", r.Run.ContextKeepRecent, 8},
		{"SummaryMaxChars", r.Run.SummaryMaxChars, 1500},
	}
	for _, c := range checks {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}

// 新字段：summary_request 和 summarizer_prompt 在 config 中正确解析。
func TestResolve_PromptFields(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a/v1/chat/completions"}},
		Defaults: DefaultsConfig{
			Model:            "p/m",
			SummaryRequest:   "自定义总结引导",
			SummarizerPrompt: "自定义压缩器",
		},
	}
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

// 缓存 parse：chatEndpoint 多次调用返回同一 *url.URL（不每请求重做，审查 v3 #10）。
func TestChatEndpoint_CachedParse(t *testing.T) {
	c := &ChatClient{ChatURL: "https://api/v1/chat/completions"}
	_, u1, err := c.chatEndpoint(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, u2, _ := c.chatEndpoint(time.Second)
	if u1 != u2 {
		t.Error("chatEndpoint should return same cached *url.URL")
	}
}

// 并发懒解析（直接 struct 构造、chatURL 未缓存）不应数据竞争（sync.Once 保护，修复 R4）。
// go test -race 下验证：多 goroutine 首次触发的懒解析无竞争，且都返回同一缓存指针。
func TestChatEndpoint_ConcurrentLazyParse(t *testing.T) {
	c := &ChatClient{ChatURL: "https://api/v1/chat/completions"}
	const n = 20
	var wg sync.WaitGroup
	seen := make([]*url.URL, n)
	for i := range seen {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, u, err := c.chatEndpoint(time.Second)
			if err != nil {
				t.Errorf("chatEndpoint: %v", err)
				return
			}
			seen[i] = u
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if seen[i] != seen[0] {
			t.Error("concurrent chatEndpoint should return same cached *url.URL")
			break
		}
	}
}

// v3.2.3 新增的 4 个策略化常量曾漏装配进 ResolvedRun（resolveRun 未赋值），config 值静默失效；
// 修复后须从 config 透传到 ResolvedRun，main 的 Set* 才能据此覆盖内置默认。
func TestResolve_StrategyConstantsLateWired(t *testing.T) {
	mk := func(v int) *int { return &v }
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a/v1/chat/completions"}},
		Defaults:  DefaultsConfig{Model: "p/m"},
		Run: RunConfig{
			SummaryMaxTokens:     mk(512),
			GrepMaxMatches:       mk(500),
			MemoryRecentN:        mk(7),
			ContextTrimToolChars: mk(1234),
		},
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
		{"SummaryMaxTokens", r.Run.SummaryMaxTokens, 512},
		{"GrepMaxMatches", r.Run.GrepMaxMatches, 500},
		{"MemoryRecentN", r.Run.MemoryRecentN, 7},
		{"ContextTrimToolChars", r.Run.ContextTrimToolChars, 1234},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}

// compaction.model 缺省时回落到主模型。
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
