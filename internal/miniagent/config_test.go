package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

func TestExpandVars_RejectControlChar(t *testing.T) {
	// 控制字符（换行/制表/回车等）裸内联会破坏 JSON 字符串结构，须拒绝。
	// （NUL 不测：OS setenv 不允许值含 NUL，进不到 expandVars。）
	for name, val := range map[string]string{
		"MA_NL": "line1\nline2",
		"MA_TB": "a\tb",
		"MA_CR": "a\rb",
	} {
		t.Setenv(name, val)
		if _, err := expandVars(`{"k":"${` + name + `}"}`); err == nil {
			t.Errorf("%s: control char %q should be rejected", name, val)
		}
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

func TestListAvailableModels_StaticNoGET(t *testing.T) {
	// ModelsURL 空 + 静态 Models → 直接返回，绝不发 HTTP（用会真实失败的内嵌 url 证明不 GET）。
	p := ProviderConfig{Name: "p", Models: []string{"a", "b"}}
	llm := &ChatClient{ChatURL: "http://127.0.0.1:1", ModelsURL: "http://127.0.0.1:1"} // 不可达
	ids, err := ListAvailableModels(context.Background(), llm, p)
	if err != nil {
		t.Fatalf("static list: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" {
		t.Errorf("ids = %v", ids)
	}
}

func TestListAvailableModels_StaticEmptyErrors(t *testing.T) {
	if _, err := ListAvailableModels(context.Background(), &ChatClient{}, ProviderConfig{Name: "p"}); err == nil {
		t.Error("empty static models should error")
	}
}

func TestListAvailableModels_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"x"},{"id":"y"}]}`)
	}))
	defer srv.Close()
	p := ProviderConfig{Name: "p", ModelsURL: srv.URL + "/v1/models"}
	llm := &ChatClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	ids, err := ListAvailableModels(context.Background(), llm, p)
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v", ids)
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
