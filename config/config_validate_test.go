package config

import (
	"testing"
)

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
	bodyOK := `{
  "providers":[
    {"name":"a","chat_url":"https://a/v1/chat/completions","models":[{"name":"m"}]},
    {"name":"b","chat_url":"https://b/v1/chat/completions","models":[{"name":"m"}],"thinking":{"field":"x-effort","map":{"fast":"low"}}}
  ],
  "defaults":{"provider":"b","model":"m","thinking":"fast"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, bodyOK)); err != nil {
		t.Errorf("defaults.thinking fast declared on b should be valid: %v", err)
	}
}

func TestLoadConfig_ThinkingPinRequiresDeclaration(t *testing.T) {
	body := `{
  "providers":[{"name":"a","chat_url":"https://a/v1/chat/completions","models":[{"name":"m"}]}],
  "defaults":{"provider":"a","model":"m","thinking":"medium"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Fatal("defaults.thinking≠off with provider not declaring thinking should error (pin: enabling thinking requires declaring {field,map})")
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
