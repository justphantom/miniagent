package main

import (
	"bufio"
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
// os.Args[0] 为 test binary 自身，用 exec.Command 重新启动它。

const entrypointEnv = "MINIAGENT_TEST_ENTRYPOINTS=1"

func TestMain(m *testing.M) {
	if os.Getenv("MINIAGENT_TEST_ENTRYPOINTS") == "1" {
		main()
		// main() 大概率已 os.Exit；若正常返回（成功完成对话路径），
		// 显式 exit 0 避免 go test 框架继续运行（行为依赖版本，是埋雷）。
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestBuildTools_AlwaysRegisters6(t *testing.T) {
	tools := buildTools(t.TempDir(), 0, "")
	if len(tools) != 9 {
		t.Fatalf("got %d tools, want 9", len(tools))
	}
	expect := map[string]bool{"read": true, "write": true, "edit": true, "multi_edit": true, "grep": true, "glob": true, "shell": true, "todo": true, "fetch": true}
	for _, tk := range tools {
		if !expect[tk.Name] {
			t.Errorf("unexpected tool %q", tk.Name)
		}
	}
}

func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("", 0, "")
	if len(tools) != 9 {
		t.Fatalf("got %d tools, want 9", len(tools))
	}
}

// runMainBin fork 出 test binary 自身，注入 env + stdin + args，返回
// (exitCode, combinedOutput)。extraEnv 追加在默认 env 之后（覆盖同名 key）。
func runMainBin(t *testing.T, stdin string, args []string, extraEnv ...string) (int, string) {
	t.Helper()
	// 用测试自身 ctx 控制 fork 出来的 binary，避免卡死。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Stdin = strings.NewReader(stdin)
	// 显式重建 env：剥离可能存在的宿主 MINIAGENT_API_KEY，保证用例独立。
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

func TestCLI_MissingModelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", nil)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "--model is required") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_MissingAPIKeyExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-model", "x"})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "API_KEY is required") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_EmptyStdinExits1(t *testing.T) {
	// 提供 API_KEY 跳过前置校验，专测 stdin empty 路径。
	code, out := runMainBin(t, "", []string{"-model", "x"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "stdin is empty") {
		t.Errorf("missing error: %s", out)
	}
}

// 超大 prompt 必须在入口处拒绝：否则写回 session 后会撞 LoadSession 的
// 大小上限，导致会话永久无法接续。
func TestCLI_OversizedStdinExits1(t *testing.T) {
	code, out := runMainBin(t, strings.Repeat("x", maxPromptBytes+1), []string{"-model", "x"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "超过大小上限") {
		t.Errorf("missing error: %s", out)
	}
}

// 两轮接续 e2e：fork 出的子进程真实请求父进程内的 httptest server。
// 第二轮的请求体必须包含第一轮的回答（上下文自动带入），session 文件
// 落盘完整 transcript。
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

	sess := filepath.Join(t.TempDir(), "s.json")
	args := []string{"-model", "m", "-base-url", srv.URL, "-session", sess}
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

// 损坏的 session 文件 → stderr 报错 + 退出码 1，不静默丢弃历史。
func TestCLI_CorruptSessionExits1(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(sess, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "prompt", []string{"-model", "x", "-session", sess}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "load session") {
		t.Errorf("missing error: %s", out)
	}
}

// 非法 -log-level 应报错退出码 1。
func TestCLI_InvalidLogLevelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-model", "x", "-log-level", "bogus"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "invalid -log-level") {
		t.Errorf("missing error: %s", out)
	}
}

// http（非 loopback）BaseURL 应警告明文传 key；loopback/https 不警告。
func TestWarnInsecureBaseURL(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	warnInsecureBaseURL("http://llm.internal:8000")
	warnInsecureBaseURL("http://localhost:8080")
	warnInsecureBaseURL("http://127.0.0.1:11434")
	warnInsecureBaseURL("http://127.0.0.2:8000") // 127.0.0.0/8 整段都是 loopback
	warnInsecureBaseURL("http://[::1]:8000")
	warnInsecureBaseURL("https://api.openai.com")
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if got := strings.Count(string(out), "warning"); got != 1 {
		t.Errorf("warnings = %d, want 1: %s", got, out)
	}
}

// resolveAPIKey：-key-file 优先于 env，首尾空白截断；缺省回退 env；读失败报错。
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
		t.Errorf("got %q, want sk-file (file trims + overrides env)", got)
	}
	got2, err := resolveAPIKey("", "sk-env2")
	if err != nil || got2 != "sk-env2" {
		t.Errorf("got (%q,%v), want sk-env2 (env fallback)", got2, err)
	}
	if _, err := resolveAPIKey(filepath.Join(dir, "nope"), ""); err == nil {
		t.Error("expected error for missing key-file")
	}
}

// key 文件权限 loose 警告，tight 不警告。
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
	warnKeyFilePerm(loose) // 应警告
	warnKeyFilePerm(tight) // 不警告
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if c := strings.Count(string(out), "warning"); c != 1 {
		t.Errorf("warnings = %d, want 1: %s", c, out)
	}
}

// e2e：仅靠 -key-file 提供 key（不设 MINIAGENT_API_KEY env），key 进入请求 Authorization。
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
	// runMainBin 已剥离 MINIAGENT_API_KEY，key 仅来自 -key-file。
	code, out := runMainBin(t, "ping", []string{"-model", "m", "-base-url", srv.URL, "-key-file", keyFile})
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-from-config-file" {
		t.Errorf("Authorization = %q, want Bearer sk-from-config-file", gotAuth)
	}
}

// 交互模式（-interactive）：stdin 两行 → 两轮对话，第二轮请求含第一轮回答
// （上下文进程内累积），session 文件含两轮内容。
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

	sess := filepath.Join(t.TempDir(), "s.json")
	code, out := runMainBin(t, "第一问\n第二问\n", []string{"-model", "m", "-base-url", srv.URL, "-interactive", "-session", sess}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2 (one per turn)", len(bodies))
	}
	// 第二轮请求须含第一轮的回答（累积）与本轮 prompt。
	if !strings.Contains(bodies[1], "回A") || !strings.Contains(bodies[1], "第二问") {
		t.Errorf("turn2 missing accumulated context: %s", bodies[1])
	}
	data, err := os.ReadFile(sess)
	if err != nil {
		t.Fatalf("session not written: %v", err)
	}
	for _, want := range []string{"第一问", "回A", "第二问"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("session missing %q: %s", want, data)
		}
	}
}

func TestCheckConfine(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.txt")
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkConfine(dir, "inner.txt"); err != nil {
		t.Errorf("inner relative path rejected: %v", err)
	}
	if err := checkConfine(dir, inner); err != nil {
		t.Errorf("inner absolute path rejected: %v", err)
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
	if err := checkConfine(dir, "link"); err == nil {
		t.Error("symlink escape should be rejected")
	}
}

// -list-models 早退：GET /v1/models，打印 id 后退出码 0。
func TestCLI_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5-turbo"}]}`)
	}))
	defer srv.Close()
	code, out := runMainBin(t, "", []string{"-list-models", "-base-url", srv.URL}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "gpt-4o") || !strings.Contains(out, "gpt-3.5-turbo") {
		t.Errorf("missing model ids: %s", out)
	}
}

// checkApprove 各 mode × 各输入（共享 reader 已修复 stdin 冲突，此处直接喂 reader）。
func TestCheckApprove(t *testing.T) {
	mkR := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }
	if err := checkApprove("all", "shell", "{}", mkR("")); err != nil {
		t.Errorf("all mode should allow: %v", err)
	}
	if err := checkApprove("dangerous", "read", "{}", mkR("")); err != nil {
		t.Errorf("non-dangerous tool should allow: %v", err)
	}
	if err := checkApprove("dangerous", "shell", "{}", mkR("y\n")); err != nil {
		t.Errorf("dangerous + y should allow: %v", err)
	}
	if err := checkApprove("dangerous", "shell", "{}", mkR("n\n")); err == nil {
		t.Error("dangerous + n should deny")
	}
	if err := checkApprove("dangerous", "shell", "{}", mkR("")); err == nil {
		t.Error("dangerous + EOF should deny")
	}
	if err := checkApprove("always", "read", "{}", mkR("y\n")); err != nil {
		t.Errorf("always + y should allow: %v", err)
	}
}

// buildTools(confine=workdir) 后，写工具对越界 path 返回 IsError（含「沙箱」）。
func TestBuildTools_ConfineRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	tools := buildTools(dir, 0, "workdir")
	byName := map[string]miniagent.Tool{}
	for _, tk := range tools {
		byName[tk.Name] = tk
	}
	for _, name := range []string{"write", "edit", "multi_edit"} {
		var args string
		if name == "write" {
			args = `{"path":"../escape.txt","content":"x"}`
		} else if name == "edit" {
			args = `{"path":"../escape.txt","old_string":"a","new_string":"b"}`
		} else {
			args = `{"path":"../escape.txt","edits":[{"old_string":"a","new_string":"b"}]}`
		}
		r := byName[name].Call(context.Background(), args)
		if !r.IsError || !strings.Contains(r.Output, "沙箱") {
			t.Errorf("%s escape should be rejected: %s", name, r.Output)
		}
	}
}
