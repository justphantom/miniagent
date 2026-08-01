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

	"github.com/justphantom/miniagent/internal/miniagent"
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

// chatArgs 构造裸模式 e2e 的公共参数（auto 模式免 workdir，handler 忽略路径）。
func chatArgs(srvURL string, extra ...string) []string {
	return append([]string{
		"-chat-url", srvURL + "/v1/chat/completions",
		"-models-url", srvURL + "/v1/models",
		"-model", "m",
		"-mode", "auto",
	}, extra...)
}

func TestBuildTools_AlwaysRegisters9(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, miniagent.ModeAuto)
	if len(tools) != 9 {
		t.Fatalf("got %d tools, want 9", len(tools))
	}
}

func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("", 0, miniagent.ModeAuto)
	if len(tools) != 9 {
		t.Fatalf("got %d tools, want 9", len(tools))
	}
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

// 裸模式无 -chat-url → 报错。
func TestCLI_NoChatURLExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", nil)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "chat-url") {
		t.Errorf("missing chat-url error: %s", out)
	}
}

// 裸模式有 -chat-url 缺 -model → 报错。
func TestCLI_MissingModelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-chat-url", "http://127.0.0.1:1/v1/chat/completions"})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "model") {
		t.Errorf("missing model error: %s", out)
	}
}

func TestCLI_MissingAPIKeyExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", chatArgs("http://127.0.0.1:1"))
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "API key") {
		t.Errorf("missing API key error: %s", out)
	}
}

// default 模式无 workdir → 报错（需 -workdir 或 -mode auto）。
func TestCLI_DefaultModeRequiresWorkdir(t *testing.T) {
	args := []string{"-chat-url", "http://127.0.0.1:1/v1/chat/completions", "-model", "m", "-mode", "default"}
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
	args := chatArgs("http://127.0.0.1:1", "-stream", "-result-only")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "互斥") {
		t.Errorf("missing mutex error: %s", out)
	}
}

func TestCLI_EmptyStdinExits1(t *testing.T) {
	code, out := runMainBin(t, "", chatArgs("http://127.0.0.1:1"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "stdin is empty") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_OversizedStdinExits1(t *testing.T) {
	code, out := runMainBin(t, strings.Repeat("x", maxPromptBytes+1), chatArgs("http://127.0.0.1:1"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "超过大小上限") {
		t.Errorf("missing error: %s", out)
	}
}

// 两轮接续 e2e：第二轮请求体含第一轮回答，session jsonl 落盘。
func TestCLI_SessionTwoTurns(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"回答X"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	sess := filepath.Join(t.TempDir(), "s.jsonl")
	args := chatArgs(srv.URL, "-session", sess)
	code, out := runMainBin(t, "第一轮提问", args, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("turn1 code = %d, out = %s", code, out)
	}
	code, out = runMainBin(t, "第二轮提问", args, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("turn2 code = %d, out = %s", code, out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("server got %d requests, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "第一轮提问") || !strings.Contains(bodies[1], "回答X") || !strings.Contains(bodies[1], "第二轮提问") {
		t.Errorf("turn2 request missing turn1 context: %s", bodies[1])
	}
	data, err := os.ReadFile(sess)
	if err != nil {
		t.Fatalf("session file not written: %v", err)
	}
	for _, want := range []string{"第一轮提问", "回答X", "第二轮提问"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("session file missing %q: %s", want, data)
		}
	}
}

// 损坏 session → 报错退出 1。
func TestCLI_CorruptSessionExits1(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "bad.jsonl")
	// 中间损坏（合法行夹非法行）：LoadSession 严格报错（尾行半写才容忍），CLI 退出码 1。
	body := "{\"type\":\"session\",\"id\":\"s\"}\n{oops}\n{\"type\":\"message\",\"role\":\"user\",\"content\":\"q\"}\n"
	if err := os.WriteFile(sess, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "prompt", chatArgs("http://127.0.0.1:1", "-session", sess), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "load session") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_InvalidLogLevelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", chatArgs("http://127.0.0.1:1", "-log-level", "bogus"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "invalid -log-level") {
		t.Errorf("missing error: %s", out)
	}
}

// http（非 loopback）端点应警告明文传 key。
func TestWarnInsecureURL(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	warnInsecureURL("http://llm.internal:8000/v1/chat/completions")
	warnInsecureURL("http://localhost:8080/v1/chat/completions")
	warnInsecureURL("http://127.0.0.1:11434/v1/chat/completions")
	warnInsecureURL("https://api.openai.com/v1/chat/completions")
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if got := strings.Count(string(out), "warning"); got != 1 {
		t.Errorf("warnings = %d, want 1: %s", got, out)
	}
}

func TestResolveAPIKey(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "k")
	if err := os.WriteFile(kf, []byte("  sk-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAPIKey(kf, "sk-env")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "sk-file" {
		t.Errorf("got %q, want sk-file", got)
	}
	got2, err := resolveAPIKey("", "sk-env2")
	if err != nil || got2 != "sk-env2" {
		t.Errorf("got (%q,%v), want sk-env2", got2, err)
	}
	if _, err := resolveAPIKey(filepath.Join(dir, "nope"), ""); err == nil {
		t.Error("expected error for missing key-file")
	}
}

func TestWarnKeyFilePerm(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	_ = os.WriteFile(loose, []byte("x"), 0o644)
	tight := filepath.Join(dir, "tight")
	_ = os.WriteFile(tight, []byte("x"), 0o600)
	warnKeyFilePerm(loose)
	warnKeyFilePerm(tight)
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if c := strings.Count(string(out), "warning"); c != 1 {
		t.Errorf("warnings = %d, want 1: %s", c, out)
	}
}

// e2e：仅靠 -key-file 提供 key，key 进入请求 Authorization。
func TestCLI_KeyFileAuth(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("sk-from-config-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "ping", chatArgs(srv.URL, "-key-file", keyFile))
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-from-config-file" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// 交互模式：两轮对话，第二轮请求含第一轮回答。
func TestCLI_InteractiveTwoTurns(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"回A"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	sess := filepath.Join(t.TempDir(), "s.jsonl")
	code, out := runMainBin(t, "第一问\n第二问\n", chatArgs(srv.URL, "-interactive", "-session", sess), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "回A") || !strings.Contains(bodies[1], "第二问") {
		t.Errorf("turn2 missing accumulated context: %s", bodies[1])
	}
}

// 薄版 checkConfine：.. 越界拒；符号链接不追（不拒）。
func TestCheckConfine(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.txt")
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkConfine(dir, "inner.txt"); err != nil {
		t.Errorf("inner relative rejected: %v", err)
	}
	if err := checkConfine(dir, filepath.Join(dir, "..", "outside")); err == nil {
		t.Error("escape via .. should be rejected")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// 薄版不追符号链接：link 路径在 workdir 子树内即放行（真隔离靠 OS）。
	if err := checkConfine(dir, "link"); err != nil {
		t.Errorf("thin checkConfine should not follow symlink: %v", err)
	}
}

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
	code, out := runMainBin(t, "", []string{"-list-models", "-chat-url", srv.URL + "/v1/chat/completions", "-models-url", srv.URL + "/v1/models"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "gpt-4o") || !strings.Contains(out, "gpt-3.5-turbo") {
		t.Errorf("missing model ids: %s", out)
	}
}

// -result-only：stdout 仅 result.text，无 NDJSON 事件。
func TestCLI_ResultOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"纯结果"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "ping", chatArgs(srv.URL, "-result-only", "-log-level", "error"), "MINIAGENT_API_KEY=sk-test")
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
	code, out := runMainBin(t, "ping", chatArgs(srv.URL, "-result-only", "-log-level", "error"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "error:") {
		t.Errorf("result-only failure should print 'error: ...': %q", out)
	}
}

// buildTools(default) 后写工具对越界 path 返回 IsError（含「default 模式」）。
func TestBuildTools_DefaultConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, miniagent.ModeDefault)
	byName := map[string]miniagent.Tool{}
	for _, tk := range tools {
		byName[tk.Name] = tk
	}
	for _, name := range []string{"write", "edit", "multi_edit"} {
		var args string
		switch name {
		case "write":
			args = `{"path":"../escape.txt","content":"x"}`
		case "edit":
			args = `{"path":"../escape.txt","old_string":"a","new_string":"b"}`
		default:
			args = `{"path":"../escape.txt","edits":[{"old_string":"a","new_string":"b"}]}`
		}
		r := byName[name].Call(context.Background(), args)
		if !r.IsError || !strings.Contains(r.Output, "default 模式") {
			t.Errorf("%s escape should be rejected: %s", name, r.Output)
		}
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
