package miniagent

import (
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
  "defaults":{"model":"main/glm","mode":"default"}
}`
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

func TestLoadConfig_CompactionModelSlash(t *testing.T) {
	body := `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":["glm"]}],
  "defaults":{"model":"main/glm"},
  "compaction":{"model":"main/glm-flash"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("compaction.model with / should error")
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
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"model":"other/m"}}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Error("defaults.model with unknown provider should error")
	}
}

func TestParseModelSpec_Slash(t *testing.T) {
	cfg := &Config{Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a"}}}
	p, id, err := ParseModelSpec("p/glm", cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Name != "p" || id != "glm" {
		t.Errorf("got p=%q id=%q", p.Name, id)
	}
}

func TestParseModelSpec_BareSingleProvider(t *testing.T) {
	cfg := &Config{Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a"}}}
	_, id, err := ParseModelSpec("glm", cfg)
	if err != nil || id != "glm" {
		t.Errorf("bare model single provider: id=%q err=%v", id, err)
	}
}

func TestParseModelSpec_BareMultiProviderErrors(t *testing.T) {
	cfg := &Config{Providers: []ProviderConfig{{Name: "p", ChatURL: "https://a"}, {Name: "q", ChatURL: "https://b"}}}
	if _, _, err := ParseModelSpec("glm", cfg); err == nil {
		t.Error("bare model with >1 provider should error")
	}
}

// S4：config run.* JSON 标签 round-trip（max_tool_result_chars 等）。
func TestLoadConfig_StrategyConstants(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"model":"p/m"},"run":{"max_tool_result_chars":1234,"max_file_result_chars":9999,"max_parallel_tools":3,"context_keep_recent":8,"summary_max_chars":1500}}`
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
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}
