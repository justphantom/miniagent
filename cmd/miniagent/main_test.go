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

func TestBuildTools_AlwaysRegisters4(t *testing.T) {
	tools := buildTools(t.TempDir())
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools))
	}
	expect := map[string]bool{"read": true, "write": true, "edit": true, "shell": true}
	for _, tk := range tools {
		if !expect[tk.Name] {
			t.Errorf("unexpected tool %q", tk.Name)
		}
	}
}

func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("")
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools))
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
