package miniagent

import (
	"os"
	"strings"
	"testing"
)

func writeTmpConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/miniagent.json"
	if err := writeFileAtomic(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func validConfigBody() string {
	return `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":["glm"]}],
  "defaults":{"provider":"main","model":"glm","mode":"default"}
}`
}

// mkFullConfig 构造 defaults/compaction 两处模型对齐全（拆分必填后）的 Config 公共固件；
// providers 缺省时补一个名为 provName 的 provider。
func mkFullConfig(provName, model string, providers ...ProviderConfig) *Config {
	if len(providers) == 0 {
		providers = []ProviderConfig{{Name: provName, ChatURL: "https://a/v1/chat/completions"}}
	}
	return &Config{
		Providers:  providers,
		Defaults:   DefaultsConfig{Provider: provName, Model: model},
		Compaction: CompactionConfig{Provider: provName, Model: model},
	}
}

func TestLoadConfig_OK(t *testing.T) {
	cfg, err := LoadConfig(writeTmpConfig(t, validConfigBody()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "main" {
		t.Errorf("providers = %+v", cfg.Providers)
	}
}

// 配置文件中不再支持 ${VAR} 注入；key 按字面量读取。
func TestLoadConfig_NoVarExpansion(t *testing.T) {
	body := strings.ReplaceAll(validConfigBody(),
		`"models":["glm"]`,
		`"models":["glm"],"key":"${MA_KEY}"`)
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers[0].Key != "${MA_KEY}" {
		t.Errorf("key should be literal, got %q", cfg.Providers[0].Key)
	}
}

func TestLoadConfig_CompactionModelCrossProvider(t *testing.T) {
	body := `{
  "providers":[
    {"name":"main","chat_url":"https://api/v1/chat/completions","models":["glm"]},
    {"name":"comp","chat_url":"https://comp/v1/chat/completions","models":["glm-flash"]}
  ],
  "defaults":{"provider":"main","model":"glm"},
  "compaction":{"provider":"comp","model":"glm-flash"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err != nil {
		t.Errorf("compaction cross-provider pair should succeed: %v", err)
	}
}

func TestLoadConfig_CompactionModelUnknownProvider(t *testing.T) {
	body := `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":["glm"]}],
  "defaults":{"provider":"main","model":"glm"},
  "compaction":{"provider":"unknown","model":"glm-flash"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("compaction.provider unknown should error")
	}
}

func TestLoadConfig_NoProviders(t *testing.T) {
	if _, err := LoadConfig(writeTmpConfig(t, `{"providers":[]}`)); err == nil {
		t.Error("empty providers should error")
	}
}

func TestLoadConfig_DupProviderName(t *testing.T) {
	body := `{"providers":[
		{"name":"p","chat_url":"https://a/v1/chat/completions"},
		{"name":"p","chat_url":"https://b/v1/chat/completions"}]}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("dup provider name should error")
	}
}

func TestLoadConfig_BadChatURL(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"ftp://x"}]}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("non-http chat_url should error")
	}
}

func TestLoadConfig_DefaultsModelUnknownProvider(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"provider":"other","model":"m"}}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("defaults.provider unknown should error")
	}
}

// L3-12：defaults.thinking 用「只在另一 provider 声明的自定义键」须在 config 校验阶段就拒
// （与 Resolve 的 per-provider 校验一致），而非聚合所有 provider 放行、留到 resolve 才失败。
func TestLoadConfig_ThinkingKeyScopedToDefaultProvider(t *testing.T) {
	body := `{
  "providers":[
    {"name":"a","chat_url":"https://a/v1/chat/completions","models":["m"]},
    {"name":"b","chat_url":"https://b/v1/chat/completions","models":["m"],"thinking":{"field":"x-effort","map":{"fast":"low"}}}
  ],
  "defaults":{"provider":"a","model":"m","thinking":"fast"}
}`
	// a 未声明 fast：config 阶段就应报错（旧聚合逻辑会放行，留到 resolve 才失败）。
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("defaults.thinking 用 a 未声明的自定义键 fast 应在 config 阶段报错（per-provider 校验）")
	}
	// 反例：defaults 指向声明了 fast 的 b 则合法。
	bodyOK := strings.Replace(body, `"provider":"a","model":"m","thinking":"fast"`, `"provider":"b","model":"m","thinking":"fast"`, 1)
	if _, err := LoadConfig(writeTmpConfig(t, bodyOK)); err != nil {
		t.Errorf("defaults 指向声明了 fast 的 b 应合法: %v", err)
	}
}

// 钉死：defaults.thinking≠off 但 provider 完全未声明 thinking → 启动期报错（强制声明 {field,map}）。
func TestLoadConfig_ThinkingPinRequiresDeclaration(t *testing.T) {
	body := `{
  "providers":[{"name":"a","chat_url":"https://a/v1/chat/completions","models":["m"]}],
  "defaults":{"provider":"a","model":"m","thinking":"medium"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Fatal("defaults.thinking≠off 且 provider 未声明 thinking 应报错（钉死：启用思考必声明 {field,map}）")
	}
}

// 成对规则：defaults 对必填；compaction 只设一个字段即报错。
func TestLoadConfig_ModelPairRequired(t *testing.T) {
	providers := `"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}]`
	def := `"defaults":{"provider":"p","model":"m"}`
	cases := map[string]string{
		"defaults.provider 缺": `{` + providers + `,"defaults":{"model":"m"}}`,
		"defaults.model 缺":    `{` + providers + `,"defaults":{"provider":"p"}}`,
		"compaction 仅 model":  `{` + providers + `,` + def + `,"compaction":{"model":"m"}}`,
	}
	for name, body := range cases {
		if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
			t.Errorf("%s: should error", name)
		}
	}
}

// compaction 整段留空合法（Resolve 时整体回落 defaults 对）。
func TestLoadConfig_SecondaryOmittedOK(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"provider":"p","model":"m"}}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err != nil {
		t.Errorf("omitted compaction should load: %v", err)
	}
}

func TestFindProvider_OK(t *testing.T) {
	cfg := &Config{Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a"}}}
	p, err := FindProvider(cfg, "p")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if p.Name != "p" {
		t.Errorf("got %q", p.Name)
	}
}

func TestFindProvider_Unknown(t *testing.T) {
	cfg := &Config{Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a"}}}
	if _, err := FindProvider(cfg, "q"); err == nil {
		t.Error("unknown provider should error")
	}
}

// S4：config run.* JSON 标签 round-trip（max_tool_result_chars 等）。
func TestLoadConfig_StrategyConstants(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"provider":"p","model":"m"},"compaction":{"provider":"p","model":"m"},"run":{"max_tool_result_chars":1234,"max_file_result_chars":9999,"max_parallel_tools":3,"context_keep_recent":8,"summary_max_chars":1500,"context_keep_reasoning":2}}`
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  *int
		want int
	}{
		{"max_tool_result_chars", cfg.Run.MaxToolResultChars, 1234},
		{"max_file_result_chars", cfg.Run.MaxFileResultChars, 9999},
		{"max_parallel_tools", cfg.Run.MaxParallelTools, 3},
		{"context_keep_recent", cfg.Run.ContextKeepRecent, 8},
		{"summary_max_chars", cfg.Run.SummaryMaxChars, 1500},
		{"context_keep_reasoning", cfg.Run.ContextKeepReasoning, 2},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}

// config.example.json 是发版旗舰示例：strip // 注释后必须可被 LoadConfig 加载
// （钉死后 openai provider 显式声明 thinking{field:reasoning_effort,map:identity}；defaults.thinking=off 不强制）。
func TestLoadConfig_ExampleFile(t *testing.T) {
	data, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if _, err := LoadConfig(writeTmpConfig(t, stripJSONComments(string(data)))); err != nil {
		t.Fatalf("config.example.json strip 注释后应可加载: %v", err)
	}
}

// stripJSONComments 去掉整行 // 注释（config.example.json 的注释均为独立行，故按行判断即可）。
func stripJSONComments(in string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(in, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// headers 字段 round-trip：provider 自定义头被正确解析。
func TestLoadConfig_ProviderHeaders(t *testing.T) {
	body := `{
  "providers":[{"name":"p","chat_url":"https://a/v1/chat/completions","headers":{"X-Custom":"abc","Authorization":"Bearer override"}}],
  "defaults":{"provider":"p","model":"m"},
  "compaction":{"provider":"p","model":"m"}
}`
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("want 1 provider")
	}
	h := cfg.Providers[0].Headers
	if len(h) != 2 || h["X-Custom"] != "abc" || h["Authorization"] != "Bearer override" {
		t.Errorf("headers = %+v", h)
	}
}
