package config

import (
	"github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"strings"
	"testing"
)

func writeFileAtomicTest(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func writeTmpConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/miniagent.json"
	if err := writeFileAtomicTest(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func validConfigBody() string {
	return `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":[{"name":"glm"}]}],
  "defaults":{"provider":"main","model":"glm","mode":"default"}
}`
}

// mkFullConfig builds a Config fixture where defaults/compaction model pairs are both aligned (required after the
// split into mandatory fields); when providers is omitted it fills in a single provider named provName.
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

func TestLoadConfig_Example(t *testing.T) {
	if _, err := LoadConfig("../../../config.example.json"); err != nil {
		t.Fatalf("load example: %v", err)
	}
}

// The config file no longer supports ${VAR} expansion; the key is read as a literal.
func TestLoadConfig_NoVarExpansion(t *testing.T) {
	body := strings.ReplaceAll(validConfigBody(),
		`"models":[{"name":"glm"}]`,
		`"models":[{"name":"glm"}],"key":"${MA_KEY}"`)
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
    {"name":"main","chat_url":"https://api/v1/chat/completions","models":[{"name":"glm"}]},
    {"name":"comp","chat_url":"https://comp/v1/chat/completions","models":[{"name":"glm-flash"}]}
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
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":[{"name":"glm"}]}],
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

func TestValidateConfig_ResponsesKind(t *testing.T) {
	cfg := mkFullConfig("main", "m", ProviderConfig{
		Name:     "main",
		Kind:     "responses",
		ChatURL:  "https://api/v1/responses",
		Thinking: &miniagent.ThinkingMapping{Field: "reasoning", Map: map[string]string{"high": "high"}},
	})
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid responses config: %v", err)
	}
	cfg.Providers[0].Thinking.Field = "reasoning_effort"
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), `thinking.field "reasoning"`) {
		t.Fatalf("err = %v, want responses thinking.field error", err)
	}
	cfg.Providers[0].Thinking.Field = "reasoning"
	cfg.Providers[0].Thinking.Map["high"] = " "
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "effort is empty") {
		t.Fatalf("err = %v, want empty effort error", err)
	}
}

func TestLoadConfig_DefaultsModelUnknownProvider(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"provider":"other","model":"m"}}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("defaults.provider unknown should error")
	}
}

// L3-12: defaults.thinking using a custom key declared only on another provider must be rejected at the config
// validation stage (consistent with Resolve's per-provider validation), rather than aggregating all providers
// and letting it pass until resolve fails.
func TestLoadConfig_ThinkingKeyScopedToDefaultProvider(t *testing.T) {
	body := `{
  "providers":[
    {"name":"a","chat_url":"https://a/v1/chat/completions","models":[{"name":"m"}]},
    {"name":"b","chat_url":"https://b/v1/chat/completions","models":[{"name":"m"}],"thinking":{"field":"x-effort","map":{"fast":"low"}}}
  ],
  "defaults":{"provider":"a","model":"m","thinking":"fast"}
}`
	// a does not declare fast: the config stage should error (old aggregation logic would let it pass, leaving resolve to fail).
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("defaults.thinking using custom key fast not declared on a should error at the config stage (per-provider validation)")
	}
	// Counter-example: pointing defaults at b, which declares fast, is valid.
	bodyOK := strings.Replace(body, `"provider":"a","model":"m","thinking":"fast"`, `"provider":"b","model":"m","thinking":"fast"`, 1)
	if _, err := LoadConfig(writeTmpConfig(t, bodyOK)); err != nil {
		t.Errorf("defaults pointing at b which declares fast should be valid: %v", err)
	}
}

// Pin: defaults.thinking≠off but the provider declares no thinking at all → startup error (forcing {field,map} declaration).
func TestLoadConfig_ThinkingPinRequiresDeclaration(t *testing.T) {
	body := `{
  "providers":[{"name":"a","chat_url":"https://a/v1/chat/completions","models":[{"name":"m"}]}],
  "defaults":{"provider":"a","model":"m","thinking":"medium"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Fatal("defaults.thinking≠off with provider not declaring thinking should error (pin: enabling thinking requires declaring {field,map})")
	}
}

// kind=anthropic: a model-level max_tokens<=0 overrides the valid provider value via pickMPG and would 400 every call,
// so it must be rejected at the config stage. nil (inherit provider) is allowed.
func TestLoadConfig_AnthropicModelMaxTokensMustBePositive(t *testing.T) {
	body := `{
  "providers":[{"name":"a","kind":"anthropic","chat_url":"https://a/v1/messages","max_tokens":64000,"models":[{"name":"m","max_tokens":0}]}],
  "defaults":{"provider":"a","model":"m"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Fatal("kind=anthropic model-level max_tokens:0 should error at config stage (overrides valid provider value)")
	}
	// Counter-example: nil model-level max_tokens inherits the provider value (valid).
	bodyOK := strings.Replace(body, `{"name":"m","max_tokens":0}`, `{"name":"m"}`, 1)
	if _, err := LoadConfig(writeTmpConfig(t, bodyOK)); err != nil {
		t.Errorf("nil model max_tokens (inherit provider 64000) should pass: %v", err)
	}
}

// kind=anthropic: models_url is now supported (upstream /v1/models uses the same x-api-key auth), so a config
// pairing kind=anthropic with models_url must pass validation and surface ModelsURL.
func TestLoadConfig_AnthropicModelsURLAllowed(t *testing.T) {
	body := `{
  "providers":[{"name":"a","kind":"anthropic","chat_url":"https://a/v1/messages","models_url":"https://a/v1/models","max_tokens":64000,"models":[{"name":"m"}]}],
  "defaults":{"provider":"a","model":"m"}
}`
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatalf("kind=anthropic + models_url should pass validation: %v", err)
	}
	if cfg.Providers[0].ModelsURL != "https://a/v1/models" {
		t.Errorf("ModelsURL = %q, want https://a/v1/models", cfg.Providers[0].ModelsURL)
	}
}

// kind=anthropic: thinking.map values are JSON-object STRINGS (resolveThinking unmarshals each). A value whose content
// is not a JSON object, or is one missing "type", must fail loud at startup rather than silently disable thinking.
func TestLoadConfig_AnthropicThinkingMapValuesValidated(t *testing.T) {
	base := func(highVal string) string {
		return `{"providers":[{"name":"a","kind":"anthropic","chat_url":"https://a/v1/messages","max_tokens":64000,"models":[{"name":"m"}],"thinking":{"field":"thinking","map":{"high":` + highVal + `}}}],"defaults":{"provider":"a","model":"m"}}`
	}
	// Each bad value is a well-formed JSON string (so map[string]string parses), but its CONTENT is either invalid JSON
	// or a JSON object missing "type".
	for _, bad := range []string{`"notvalidjson"`, `"{\"effort\":\"high\"}"`} {
		if _, err := LoadConfig(writeTmpConfig(t, base(bad))); err == nil {
			t.Errorf("anthropic thinking.map high=%q should error (content not a JSON object with type)", bad)
		}
	}
	if _, err := LoadConfig(writeTmpConfig(t, base(`"{\"type\":\"adaptive\",\"effort\":\"high\"}"`))); err != nil {
		t.Errorf("valid anthropic thinking.map value should pass: %v", err)
	}
}

// Pairing rule: the defaults pair is required; setting only one compaction field errors.
func TestLoadConfig_ModelPairRequired(t *testing.T) {
	providers := `"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}]`
	def := `"defaults":{"provider":"p","model":"m"}`
	cases := map[string]string{
		"defaults.provider missing": `{` + providers + `,"defaults":{"model":"m"}}`,
		"defaults.model missing":    `{` + providers + `,"defaults":{"provider":"p"}}`,
		"compaction model-only":     `{` + providers + `,` + def + `,"compaction":{"model":"m"}}`,
	}
	for name, body := range cases {
		if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
			t.Errorf("%s: should error", name)
		}
	}
}

// An entirely empty compaction section is valid (Resolve falls back to the defaults pair as a whole).
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

// S4: config run.* JSON tag round-trip (max_tool_result_chars etc.).
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

// config.example.json is the release flagship sample: after stripping // comments it must be loadable by LoadConfig
// (after pinning, the openai provider explicitly declares thinking{field:reasoning_effort,map:identity}; defaults.thinking=off is not enforced).
func TestLoadConfig_ExampleFile(t *testing.T) {
	data, err := os.ReadFile("../../../config.example.json")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if _, err := LoadConfig(writeTmpConfig(t, stripJSONComments(string(data)))); err != nil {
		t.Fatalf("config.example.json should be loadable after stripping comments: %v", err)
	}
}

// stripJSONComments removes whole-line // comments (all comments in config.example.json are on standalone lines, so per-line checks suffice).
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

// headers field round-trip: provider custom headers are parsed correctly.
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

// breaking: the models field must be object-form [{"name":...}]; the legacy string form ["x"] fails to load.
func TestLoadConfig_ModelsMustBeObjects(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions","models":["legacy-string"]}],"defaults":{"provider":"p","model":"m"}}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("legacy string-form models should fail to load (breaking schema: models must be objects)")
	}
}

// Prompt field round-trip: subagent_guidance/summary_create_instruction/summary_update_instruction/summary_template.
func TestLoadConfig_PromptFields(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions","models":[{"name":"m"}]}],"defaults":{"provider":"p","model":"m","subagent_guidance":"G{config_path}","summary_create_instruction":"C{max_chars}","summary_update_instruction":"U{max_chars}","summary_template":"T"},"compaction":{"provider":"p","model":"m"}}`
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"SubagentGuidance", cfg.Defaults.SubagentGuidance, "G{config_path}"},
		{"SummaryCreateInstruction", cfg.Defaults.SummaryCreateInstruction, "C{max_chars}"},
		{"SummaryUpdateInstruction", cfg.Defaults.SummaryUpdateInstruction, "U{max_chars}"},
		{"SummaryTemplate", cfg.Defaults.SummaryTemplate, "T"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
