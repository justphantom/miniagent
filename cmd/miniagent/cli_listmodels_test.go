package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// modelLine 是 -list-models 输出的 NDJSON 事件（与 events.go modelEvent 同构）。
type modelLine struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// parseModelLines 从输出中提取 type=model 的 NDJSON 事件；非 JSON 行（stderr 文本）跳过，
// 因为 runMainBin 把 stdout/stderr 合流（部分失败场景两者混杂）。
func parseModelLines(t *testing.T, out string) []modelLine {
	t.Helper()
	var evs []modelLine
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var ev modelLine
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type != "model" {
			continue
		}
		evs = append(evs, ev)
	}
	return evs
}

// -list-models：GET models-url，逐行输出 NDJSON {"type":"model","provider","model"}。
func TestCLI_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5-turbo"}]}`)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "", []string{"-list-models", "-config", writeConfigFixture(t, srv.URL, "")}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	evs := parseModelLines(t, out)
	if len(evs) != 2 {
		t.Fatalf("want 2 model events, got %d: %s", len(evs), out)
	}
	got := map[string]bool{}
	for _, ev := range evs {
		if ev.Provider != "p" {
			t.Errorf("provider = %q, want p", ev.Provider)
		}
		got[ev.Model] = true
	}
	if !got["gpt-4o"] || !got["gpt-3.5-turbo"] {
		t.Errorf("missing models: %+v", evs)
	}
}

// -list-models 多 provider：聚合所有 provider 的模型列表，NDJSON 事件带各自 provider 字段。
func TestCLI_ListModels_MultiProvider(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"deepseek-chat"}]}`)
	}))
	defer srv2.Close()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := fmt.Sprintf(`{"providers":[{"name":"p1","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"},{"name":"p2","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"}],"defaults":{"provider":"p1","model":"x"},"compaction":{"provider":"p1","model":"x"}}`, srv1.URL, srv1.URL, srv2.URL, srv2.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "", []string{"-list-models", "-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	evs := parseModelLines(t, out)
	want := map[modelLine]bool{
		{Type: "model", Provider: "p1", Model: "gpt-4o"}:        true,
		{Type: "model", Provider: "p2", Model: "deepseek-chat"}: true,
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %s", len(evs), out)
	}
	for _, ev := range evs {
		if !want[ev] {
			t.Errorf("unexpected event: %+v", ev)
		}
	}
}

// -list-models 多 provider + -provider：仅列出指定 provider，仍输出前缀格式。
func TestCLI_ListModels_MultiProvider_ProviderFilter(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"deepseek-chat"}]}`)
	}))
	defer srv2.Close()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := fmt.Sprintf(`{"providers":[{"name":"p1","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"},{"name":"p2","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"}],"defaults":{"provider":"p1","model":"x"},"compaction":{"provider":"p1","model":"x"}}`, srv1.URL, srv1.URL, srv2.URL, srv2.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "", []string{"-list-models", "-provider", "p2", "-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	evs := parseModelLines(t, out)
	if len(evs) != 1 || evs[0].Provider != "p2" || evs[0].Model != "deepseek-chat" {
		t.Errorf("want only p2/deepseek-chat event, got %+v (out: %s)", evs, out)
	}
}

// -list-models 多 provider 部分失败：成功 provider 的 id 仍打印到 stdout，最终退出码 1。
func TestCLI_ListModels_MultiProvider_PartialFailure(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusInternalServerError)
	}))
	defer srv2.Close()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := fmt.Sprintf(`{"providers":[{"name":"ok","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"},{"name":"fail","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"}],"defaults":{"provider":"ok","model":"x"},"compaction":{"provider":"ok","model":"x"}}`, srv1.URL, srv1.URL, srv2.URL, srv2.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "", []string{"-list-models", "-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	evs := parseModelLines(t, out)
	if len(evs) != 1 || evs[0].Provider != "ok" || evs[0].Model != "model-a" {
		t.Errorf("want ok/model-a event, got %+v", evs)
	}
	if !strings.Contains(out, "fail") {
		t.Errorf("error should mention failing provider: %s", out)
	}
}

// -result-only：stdout 仅 result.text，无 NDJSON 事件。
func TestCLI_ResultOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"纯结果"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "ping", configArgs(t, srv.URL, "-result-only", "-log-level", "error"), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if strings.TrimSpace(out) != "纯结果" {
		t.Errorf("result-only stdout should be bare text, got: %q", out)
	}
}

// -result-only 失败：输出 "error: <msg>" + 退出码 1。
func TestCLI_ResultOnlyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "ping", configArgs(t, srv.URL, "-result-only", "-log-level", "error"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "error:") {
		t.Errorf("result-only failure should print 'error: ...': %q", out)
	}
}

// config 模式 e2e：system prompt 含 subagent 引导（config 绝对路径 + 无状态 fork 命令）。
// subagent 改无状态后不再注入父 session id——无状态调用即触发 guidance 注入。
func TestCLI_SubagentPromptInjected(t *testing.T) {
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	cfg := `{"providers":[{"name":"main","chat_url":"` + srv.URL + `/v1/chat/completions","models":["glm"]}],"defaults":{"provider":"main","model":"glm","mode":"auto"},"compaction":{"provider":"main","model":"glm"}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "hi", []string{"-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	abs, _ := filepath.Abs(cfgPath)
	if !strings.Contains(body, abs) {
		t.Errorf("system prompt missing config abs path %q: %s", abs, body)
	}
	if !strings.Contains(body, "无状态") || !strings.Contains(body, "subagent") {
		t.Errorf("system prompt missing subagent stateless guidance: %s", body)
	}
}
