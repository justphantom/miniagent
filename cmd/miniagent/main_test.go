package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fork-based 测试：通过 MINIAGENT_TEST_ENTRYPOINTS=1 让 test binary 重新进入
// main()，覆盖 os.Exit 路径（这些路径无法在进程内测试）。

const entrypointEnv = "MINIAGENT_TEST_ENTRYPOINTS=1"

func TestMain(m *testing.M) {
	if os.Getenv("MINIAGENT_TEST_ENTRYPOINTS") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writeConfigFixture 写一份指向 srvURL 的临时 miniagent.json（mode=auto 免 workdir），
// 返回其路径。runJSON 非空时原样作为 "run" 段（支持 max_tokens_total/max_duration 等仅 config 参数）。
func writeConfigFixture(t *testing.T, srvURL, runJSON string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	runField := ""
	if runJSON != "" {
		runField = `,"run":` + runJSON
	}
	body := `{"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions","models_url":"` + srvURL + `/v1/models"}],"defaults":{"model":"p/m","mode":"auto"}` + runField + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// configArgs 构造 e2e 的公共参数：写临时 config（替代旧裸模式 chatArgs），返回 -config <path> + extra。
func configArgs(t *testing.T, srvURL string, extra ...string) []string {
	t.Helper()
	return append([]string{"-config", writeConfigFixture(t, srvURL, "")}, extra...)
}

// runMainBin fork 出 test binary 自身，注入 env + stdin + args，返回
// (exitCode, combinedOutput)。extraEnv 追加在默认 env 之后（覆盖同名 key）。
func runMainBin(t *testing.T, stdin string, args []string, extraEnv ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Stdin = strings.NewReader(stdin)
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") ||
			strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec test binary: %v", err)
	}
	return code, out.String()
}

func TestCLI_VersionExitsZero(t *testing.T) {
	code, out := runMainBin(t, "", []string{"-version"})
	if code != 0 {
		t.Errorf("code = %d, want 0; out=%s", code, out)
	}
	if !strings.Contains(out, "miniagent ") {
		t.Errorf("missing version banner: %s", out)
	}
}

// 显式 -config 不存在 → 报错（S1 后 config 必须存在，不再退裸模式）。
func TestCLI_ExplicitConfigMissingExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-config", filepath.Join(t.TempDir(), "nope.json")})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "config") {
		t.Errorf("missing config error: %s", out)
	}
}

// config 无 defaults.model 且未传 -model → Resolve 报错。
func TestCLI_MissingModelExits1(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"providers":[{"name":"p","chat_url":"http://127.0.0.1:1/v1/chat/completions"}]}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "model") {
		t.Errorf("missing model error: %s", out)
	}
}

func TestCLI_MissingAPIKeyExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", configArgs(t, "http://127.0.0.1:1"))
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "API key") {
		t.Errorf("missing API key error: %s", out)
	}
}

// default 模式无 workdir → 报错（需 -workdir 或 -mode auto）。
func TestCLI_DefaultModeRequiresWorkdir(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1", "-mode", "default")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "workdir") {
		t.Errorf("missing workdir-required error: %s", out)
	}
}

// -stream 与 -result-only 互斥。
func TestCLI_StreamResultOnlyMutex(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1", "-stream", "-result-only")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "互斥") {
		t.Errorf("missing mutex error: %s", out)
	}
}

func TestCLI_EmptyStdinExits1(t *testing.T) {
	code, out := runMainBin(t, "", configArgs(t, "http://127.0.0.1:1"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "stdin is empty") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_OversizedStdinExits1(t *testing.T) {
	code, out := runMainBin(t, strings.Repeat("x", maxPromptBytes+1), configArgs(t, "http://127.0.0.1:1"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "超过大小上限") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_InvalidLogLevelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", configArgs(t, "http://127.0.0.1:1", "-log-level", "bogus"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "invalid -log-level") {
		t.Errorf("missing error: %s", out)
	}
}

// e2e：单次非交互运行带 -session 会把对话追加到 session 文件。
func TestCLI_SingleTurnSessionAppend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	sessPath := filepath.Join(t.TempDir(), "s.jsonl")
	args := configArgs(t, srv.URL, "-session", sessPath)
	code, out := runMainBin(t, "hi", args, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if !strings.Contains(out, `"type":"result"`) || !strings.Contains(out, `"text":"ok"`) {
		t.Errorf("missing result event: %s", out)
	}
	data, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if !strings.Contains(string(data), `"role":"user"`) || !strings.Contains(string(data), `"role":"assistant"`) {
		t.Errorf("session missing messages: %s", data)
	}
}

// e2e：stdin 全部内容作为一个 turn 的完整 prompt（多行不拆分），一次 LLM 调用返回一个 result。
func TestCLI_MultiLineStdinSingleTurn(t *testing.T) {
	var calls int
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	code, out := runMainBin(t, "hi\nhello\n", configArgs(t, srv.URL), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1（多行 stdin 应合成一个 turn）", calls)
	}
	if strings.Count(out, `"type":"result"`) != 1 {
		t.Errorf("want 1 result event, got: %s", out)
	}
	if !strings.Contains(gotBody, `hi\nhello`) {
		t.Errorf("prompt 应保留多行原文: %s", gotBody)
	}
}

// e2e：run.max_duration 到期时非交互路径返回 1。
func TestCLI_MaxDurationExits1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	cfgPath := writeConfigFixture(t, srv.URL, `{"max_duration":"1ns"}`)
	code, out := runMainBin(t, "hi", []string{"-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1; out=%s", code, out)
	}
}

// e2e：config provider.key 提供 key，进入请求 Authorization。
func TestCLI_ProviderKeyAuth(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"providers":[{"name":"p","chat_url":"` + srv.URL + `/v1/chat/completions","key":"sk-from-config"}],"defaults":{"model":"p/m","mode":"auto"}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "ping", []string{"-config", cfgPath})
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-from-config" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// e2e：会话结束自动抽取记忆。主模型先回 tool_call 再回 final；抽取调用（请求体无 tools）
// 回 JSON 记录，验证 workdir/.miniagent/memory.jsonl 被写入。
func TestCLI_AutoMemoryExtract(t *testing.T) {
	var toolCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if !strings.Contains(string(body), `"tools"`) {
			// 抽取调用（无 tools 字段）：content 内放 JSON 数组。
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"[{\"type\":\"note\",\"topic\":\"构建\",\"content\":\"go build 用 ./...\"}]"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		toolCalls++
		if toolCalls == 1 {
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"write","arguments":"{\"path\":\"a.txt\",\"content\":\"hello\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	workdir := t.TempDir()
	code, out := runMainBin(t, "跑构建并总结", configArgs(t, srv.URL, "-workdir", workdir), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	data, err := os.ReadFile(filepath.Join(workdir, ".miniagent", "memory.jsonl"))
	if err != nil {
		t.Fatalf("memory file not written: %v\nout=%s", err, out)
	}
	if !strings.Contains(string(data), "go build 用 ./...") {
		t.Errorf("expected extracted memory, got: %s", data)
	}
}
