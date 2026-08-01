package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExpandVars(t *testing.T) {
	t.Setenv("MA_TEST_K", "sk-123")
	got, err := expandVars(`{"key":"${MA_TEST_K}"}`)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(got, "sk-123") {
		t.Errorf("not expanded: %s", got)
	}
}

func TestExpandVars_UnsetErrors(t *testing.T) {
	if _, err := expandVars(`${MA_NOPE_XYZ}`); err == nil {
		t.Error("unset var should error")
	}
}

func TestExpandVars_EmptyErrors(t *testing.T) {
	t.Setenv("MA_EMPTY", "")
	if _, err := expandVars(`${MA_EMPTY}`); err == nil {
		t.Error("empty var should error")
	}
}

func TestExpandVars_RejectSpecialChar(t *testing.T) {
	t.Setenv("MA_QUOTE", `a"b`)
	if _, err := expandVars(`${MA_QUOTE}`); err == nil {
		t.Error("quote in value should be rejected")
	}
}

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

func TestLoadConfig_VarExpanded(t *testing.T) {
	t.Setenv("MA_KEY", "sk-expanded")
	body := strings.ReplaceAll(validConfigBody(),
		`"models":["glm"]`,
		`"models":["glm"],"key":"${MA_KEY}"`)
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers[0].Key != "sk-expanded" {
		t.Errorf("key not expanded: %q", cfg.Providers[0].Key)
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

func TestResolve_BareModeImplicitProvider(t *testing.T) {
	chat := "https://api/v1/chat/completions"
	model := "glm-5.2"
	r, err := Resolve(nil, CLIOverrides{ChatURL: &chat, Model: &model})
	if err != nil {
		t.Fatalf("resolve bare: %v", err)
	}
	if r.Provider.Name != "cli" || r.Provider.ChatURL != chat || r.ModelID != model {
		t.Errorf("bare provider wrong: %+v", r.Provider)
	}
}

func TestResolve_BareModeRequiresChatURL(t *testing.T) {
	model := "glm"
	if _, err := Resolve(nil, CLIOverrides{Model: &model}); err == nil {
		t.Error("bare mode without chat-url should error")
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

func TestListAvailableModels_StaticNoGET(t *testing.T) {
	// ModelsURL 空 + 静态 Models → 直接返回，绝不发 HTTP（用会真实失败的内嵌 url 证明不 GET）。
	p := ProviderConfig{Name: "p", Models: []string{"a", "b"}}
	llm := &HTTPClient{ChatURL: "http://127.0.0.1:1", ModelsURL: "http://127.0.0.1:1"} // 不可达
	ids, err := ListAvailableModels(context.Background(), llm, p)
	if err != nil {
		t.Fatalf("static list: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" {
		t.Errorf("ids = %v", ids)
	}
}

func TestListAvailableModels_StaticEmptyErrors(t *testing.T) {
	if _, err := ListAvailableModels(context.Background(), &HTTPClient{}, ProviderConfig{Name: "p"}); err == nil {
		t.Error("empty static models should error")
	}
}

func TestListAvailableModels_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"x"},{"id":"y"}]}`)
	}))
	defer srv.Close()
	p := ProviderConfig{Name: "p", ModelsURL: srv.URL + "/v1/models"}
	llm := &HTTPClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	ids, err := ListAvailableModels(context.Background(), llm, p)
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v", ids)
	}
}

// 缓存 parse：chatEndpoint 多次调用返回同一 *url.URL（不每请求重做，审查 v3 #10）。
func TestChatEndpoint_CachedParse(t *testing.T) {
	c := &HTTPClient{ChatURL: "https://api/v1/chat/completions"}
	_, u1, err := c.chatEndpoint(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, u2, _ := c.chatEndpoint(time.Second)
	if u1 != u2 {
		t.Error("chatEndpoint should return same cached *url.URL")
	}
}
