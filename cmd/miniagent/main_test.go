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
	"sync/atomic"
	"syscall"
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

func TestBuildTools_AlwaysRegisters6(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, 0, 0, miniagent.ModeAuto, 0, nil)
	if len(tools) != 6 {
		t.Fatalf("got %d tools, want 6", len(tools))
	}
}

func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("", 0, 0, 0, miniagent.ModeAuto, 0, nil)
	if len(tools) != 6 {
		t.Fatalf("got %d tools, want 6", len(tools))
	}
}

// S4：fileResultLimit>0 覆盖 read/edit 的 ResultLimit；<=0 保留构造器内置默认。
func TestBuildTools_FileResultLimitOverride(t *testing.T) {
	dir := t.TempDir()
	byName := map[string]int{}
	for _, tl := range buildTools(dir, 0, 0, 0, miniagent.ModeAuto, 4242, nil) {
		byName[tl.Name] = tl.ResultLimit
	}
	for _, name := range []string{"read", "edit"} {
		if byName[name] != 4242 {
			t.Errorf("%s ResultLimit = %d, want 4242", name, byName[name])
		}
	}
	// <=0：保留内置 maxFileResultInHistory（8000）。
	for _, tl := range buildTools(dir, 0, 0, 0, miniagent.ModeAuto, 0, nil) {
		if tl.Name == "read" && tl.ResultLimit != 8000 {
			t.Errorf("read ResultLimit = %d, want builtin 8000 when limit<=0", tl.ResultLimit)
		}
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
	args := configArgs(t, srv.URL, "-session", sess)
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
	code, out := runMainBin(t, "prompt", configArgs(t, "http://127.0.0.1:1", "-session", sess), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "load session") {
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
	code, out := runMainBin(t, "ping", configArgs(t, srv.URL, "-key-file", keyFile))
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
	code, out := runMainBin(t, "第一问\n第二问\n", configArgs(t, srv.URL, "-interactive", "-session", sess), "MINIAGENT_API_KEY=sk-test")
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
	code, out := runMainBin(t, "", []string{"-list-models", "-config", writeConfigFixture(t, srv.URL, "")}, "MINIAGENT_API_KEY=sk-test")
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

// buildTools(default) 后写工具对越界 path 返回 IsError（含「default 模式」）。
func TestBuildTools_DefaultConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, 0, 0, miniagent.ModeDefault, 0, nil)
	byName := map[string]miniagent.Tool{}
	for _, tk := range tools {
		byName[tk.Name] = tk
	}
	cases := []struct{ name, args string }{
		{"write", `{"path":"../escape.txt","content":"x"}`},
		{"edit", `{"path":"../escape.txt","old_string":"a","new_string":"b"}`},
		{"edit", `{"path":"../escape.txt","edits":[{"old_string":"a","new_string":"b"}]}`},
	}
	for _, c := range cases {
		r := byName[c.name].Call(context.Background(), c.args)
		if !r.IsError || !strings.Contains(r.Output, "default 模式") {
			t.Errorf("%s escape should be rejected: %s", c.name, r.Output)
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

// requireConfig：默认 ./miniagent.json 不存在时写最小模板再加载。须切到临时 cwd 控制。
// 模板用 ${CHAT_URL}/${MODEL}，设置 env 后加载成功，且 provider.chat_url 来自 env。
func TestRequireConfig_DefaultMissingWritesTemplate(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHAT_URL", "http://localhost:11434/v1/chat/completions")
	t.Setenv("MODEL", "gpt-4o") // 模板 provider 名为 default，MODEL 须为裸 id（无 provider 前缀）
	cfg, err := requireConfig("")
	if err != nil {
		t.Fatalf("requireConfig: %v", err)
	}
	if cfg == nil || cfg.Providers[0].ChatURL != "http://localhost:11434/v1/chat/completions" {
		t.Errorf("template not loaded: %+v", cfg)
	}
	if _, statErr := os.Stat("./miniagent.json"); statErr != nil {
		t.Errorf("template file not written: %v", statErr)
	}
}

// requireConfig：默认 config 的 Stat 错误若非 fs.ErrNotExist（如自指符号链接触发
// ELOOP、permission denied）按硬错误返回（审查 P2-6）。
func TestRequireConfig_DefaultStatErrorIsHardError(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 自指符号链接：Stat 跟随 → ELOOP（非 fs.ErrNotExist）→ 硬错误。
	if err := os.Symlink("./miniagent.json", "./miniagent.json"); err != nil {
		t.Skipf("cannot create self-referential symlink: %v", err)
	}
	if _, err := requireConfig(""); err == nil {
		t.Fatal("expected hard error for non-ErrNotExist Stat failure, got nil")
	}
}

// checkConfine 拒绝 path="." 或等于 workdir 绝对路径：rename 覆盖目录会 EISDIR
// （错误含糊），且 MkdirAll/Rename 真生效将摧毁整个 workdir（审查 P3-8）。
func TestCheckConfine_RejectsWorkdirRoot(t *testing.T) {
	dir := t.TempDir()
	if err := checkConfine(dir, "."); err == nil {
		t.Error(`path="." should be rejected as workdir root`)
	}
	if err := checkConfine(dir, dir); err == nil {
		t.Error("absolute path equal to workdir root should be rejected")
	}
}

// P2 交互无跨轮预算：-interactive 模式跨轮累计 usage 超 -max-tokens-total 时停止交互循环
// （emit error + exit 1，与单轮 ErrBudgetExceeded 语义对齐）。单轮上限仍由 Run 管。
func TestCLI_InteractiveCrossTurnBudgetStops(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		// 每轮 prompt=8 completion=2 共 10；-max-tokens-total=15 → 第二轮累计 20>15 停止。
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "q1\nq2\nq3\n", []string{"-config", writeConfigFixture(t, srv.URL, `{"max_tokens_total":15}`), "-interactive"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1（跨轮预算超限 exit 1）; out=%s", code, out)
	}
	if !strings.Contains(out, "跨轮累计") {
		t.Errorf("missing 跨轮累计 error: %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	// 应在第二轮后停止（第三轮 q3 不应再发请求）。
	if len(bodies) > 2 {
		t.Errorf("应在第二轮后停止，但发了 %d 请求", len(bodies))
	}
}

// P2 thinking 跨轮固化（interact 侧）：第一轮 thinking 不支持降级后，第二轮请求不再含
// reasoning_effort 字段。阶段 1 在 Run 内已固化（同轮多步不重撞 400），此测试覆盖跨轮：
// interact 据 result.ThinkingDowngraded 清 baseCfg.ThinkingLevel。
func TestCLI_InteractiveThinkingDowngradePersists(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		idx := callCount.Add(1)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		// 首次请求含 thinking → 返 400 reasoning_effort not supported。
		if idx == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"reasoning_effort not supported","type":"invalid_request_error"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "q1\nq2\n", configArgs(t, srv.URL, "-interactive", "-thinking", "medium"), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	// bodies[0]=q1 thinking(400), bodies[1]=q1 retry no thinking, bodies[2]=q2 no thinking。
	if len(bodies) < 3 {
		t.Fatalf("bodies=%d: %s", len(bodies), bodies)
	}
	if !strings.Contains(bodies[2], "q2") {
		t.Fatalf("bodies[2] 应为 q2 请求: %s", bodies[2])
	}
	if strings.Contains(bodies[2], "reasoning_effort") {
		t.Errorf("第二轮仍含 reasoning_effort（thinking 跨轮未固化）: %s", bodies[2])
	}
}

// P3 SIGINT 退出码：SIGINT 走码 130（128+SIGINT POSIX），不 emit error 事件（干净退出）。
// 非流式 Do 在 ctx cancel 后返回 context.Canceled，main 据此 os.Exit(130)。
func TestCLI_SIGINTExits130(t *testing.T) {
	var hitOnce sync.Once
	serverHit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOnce.Do(func() { close(serverHit) }) // 子进程已进入 Run 并发出 HTTP
		time.Sleep(5 * time.Second)             // 慢响应让 SIGINT 在响应前到达
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], configArgs(t, srv.URL)...)
	cmd.Stdin = strings.NewReader("prompt")
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") || strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "MINIAGENT_API_KEY=sk-test")
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// 等 srv 收到请求（子进程已进入 Run 的 HTTP 阻塞）再发 SIGINT：固定 sleep 在 -race
	// 负载下不可靠——子进程可能尚未注册 signal handler，SIGINT 被默认 disposition 杀死（exit -1）。
	select {
	case <-serverHit:
	case <-ctx.Done():
		t.Fatalf("子进程未在超时内打 srv: %s", out.String())
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	var ee *exec.ExitError
	code := 0
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("cmd.Wait err=%v out=%s", err, out.String())
	}
	if code != 130 {
		t.Errorf("SIGINT 应退出 130, got %d (out=%s)", code, out.String())
	}
	// SIGINT 不应 emit error NDJSON 事件（干净退出，区别于真故障）。
	if strings.Contains(out.String(), `"type":"error"`) {
		t.Errorf("SIGINT 不应 emit error 事件: %s", out.String())
	}
}
