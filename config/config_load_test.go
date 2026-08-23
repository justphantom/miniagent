package config

import (
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
  "defaults":{"provider":"main","model":"glm"}
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
	if _, err := LoadConfig("../config.example.json"); err != nil {
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

// A config typo (e.g. chat_url → chaturl, or a stale key from a removed version) must fail loudly, not be
// silently dropped into a zero-value field — otherwise the run proceeds with unintended config. Regression for
// decodeConfigStrict's DisallowUnknownFields.
func TestLoadConfig_UnknownFieldRejected(t *testing.T) {
	for _, body := range []string{
		`{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions","chaturl":"https://x"}]}`,
		`{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions","tokens":100}]}`, // stale key
		`{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"unknown_top":1}`,
	} {
		if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
			t.Errorf("config with unknown field should error: %s", body)
		}
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

// v5.0.0 removed keys (defaults.mode / provider.cache / run.confine_* ) must be tolerated in old configs —
// the CHANGELOG promises "旧 config 无需改即可加载". They are kept as json fields (silently ignored) so
// DisallowUnknownFields does not reject them, while genuine typos (chaturl) still fail.
func TestLoadConfig_OldRemovedKeysTolerated(t *testing.T) {
	base := func(run string) string {
		return `{"providers":[{"name":"main","chat_url":"https://a/v1/chat/completions"` + run + `}],"defaults":{"provider":"main","model":"m","mode":"default"}}`
	}
	for _, body := range []string{
		base(``),               // defaults.mode
		base(`, "cache":true`), // provider.cache
		`{"providers":[{"name":"main","chat_url":"https://a/v1/chat/completions"}],"defaults":{"provider":"main","model":"m"},"run":{"confine_eval_symlinks":true,"confine_auto":false}}`,
	} {
		if _, err := LoadConfig(writeTmpConfig(t, body)); err != nil {
			t.Errorf("old config with removed key should load: %s → %v", body, err)
		}
	}
}

func TestLoadConfig_BadChatURL(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"ftp://x"}]}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("non-http chat_url should error")
	}
}
