package main

import (
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

// -list-models：GET models-url，打印 id。
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
	if !strings.Contains(out, "gpt-4o") || !strings.Contains(out, "gpt-3.5-turbo") {
		t.Errorf("missing model ids: %s", out)
	}
}

// -list-models 多 provider：聚合所有 provider 的模型列表，输出 "provider/model_id" 格式。
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
	body := fmt.Sprintf(`{"providers":[{"name":"p1","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"},{"name":"p2","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"}]}`, srv1.URL, srv1.URL, srv2.URL, srv2.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "", []string{"-list-models", "-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "p1/gpt-4o") || !strings.Contains(out, "p2/deepseek-chat") {
		t.Errorf("missing aggregated ids: %s", out)
	}
}

// -list-models 多 provider + -model：仅列出指定 provider，仍输出前缀格式。
func TestCLI_ListModels_MultiProvider_ModelFilter(t *testing.T) {
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
	body := fmt.Sprintf(`{"providers":[{"name":"p1","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"},{"name":"p2","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"}]}`, srv1.URL, srv1.URL, srv2.URL, srv2.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "", []string{"-list-models", "-model", "p2/x", "-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "p2/deepseek-chat") {
		t.Errorf("missing filtered id: %s", out)
	}
	if strings.Contains(out, "p1/gpt-4o") {
		t.Errorf("should not contain p1 ids: %s", out)
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
	body := fmt.Sprintf(`{"providers":[{"name":"ok","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"},{"name":"fail","chat_url":"%s/v1/chat/completions","models_url":"%s/v1/models"}]}`, srv1.URL, srv1.URL, srv2.URL, srv2.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "", []string{"-list-models", "-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "ok/model-a") {
		t.Errorf("missing successful provider id: %s", out)
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

// config 模式 e2e：system prompt 含 subagent 引导（config 绝对路径 + 父 session id）。
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
	cfg := `{"providers":[{"name":"main","chat_url":"` + srv.URL + `/v1/chat/completions","models":["glm"]}],"defaults":{"model":"main/glm","mode":"auto"}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sessID := "parent1"
	code, out := runMainBin(t, "hi", []string{"-config", cfgPath, "-session", sessID}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	abs, _ := filepath.Abs(cfgPath)
	if !strings.Contains(body, abs) {
		t.Errorf("system prompt missing config abs path %q: %s", abs, body)
	}
	if !strings.Contains(body, sessID+"-sub-") {
		t.Errorf("system prompt missing parent session id guidance: %s", body)
	}
}
