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
	"syscall"
	"testing"
	"time"
)

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
