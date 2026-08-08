package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// writeSessionConfig 写一份指向 srvURL 的临时 config，session.dir=sessionDir，mode=auto（免 workdir）。
func writeSessionConfig(t *testing.T, srvURL, sessionDir string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"session":{"dir":"` + sessionDir + `"},"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions","models_url":"` + srvURL + `/v1/models"}],"defaults":{"provider":"p","model":"m","mode":"auto"},"compaction":{"provider":"p","model":"m"}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// 两轮接续 e2e：turn1 -save-session 新建（id 内部生成），turn2 -session <id> 接续。
// 验证第二轮请求体含第一轮回答，且 session jsonl 落盘两轮内容。
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

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "第一轮提问", []string{"-config", cfgPath, "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("turn1 code = %d, out = %s", code, out)
	}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 session file, got %v (err=%v) out=%s", matches, err, out)
	}
	sess := matches[0]
	id := strings.TrimSuffix(filepath.Base(sess), ".jsonl")

	code, out = runMainBin(t, "第二轮提问", []string{"-config", cfgPath, "-session", id}, "MINIAGENT_API_KEY=sk-test")
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

// 损坏 session → 接续时报错退出 1。
func TestCLI_CorruptSessionExits1(t *testing.T) {
	sessionDir := t.TempDir()
	sess := filepath.Join(sessionDir, "bad.jsonl")
	// 中间损坏（合法行夹非法行）：LoadSession 严格报错（尾行半写才容忍），CLI 退出码 1。
	body := "{\"type\":\"session\",\"id\":\"bad\"}\n{oops}\n{\"type\":\"message\",\"role\":\"user\",\"content\":\"q\"}\n"
	if err := os.WriteFile(sess, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath, "-session", "bad"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "load session") {
		t.Errorf("missing error: %s", out)
	}
}

// -session 指向不存在的会话 → 报错退出 1（防 typo 建垃圾会话；新建须 -save-session）。
func TestCLI_ResumeMissingSessionExits1(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath, "-session", "nope"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "不存在") {
		t.Errorf("missing not-exist error: %s", out)
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

// -save-session 与 -result-only 同传 → stderr 报错退出 1（subagent fork 无状态、不落盘会话）。
func TestCLI_SaveSessionAndResultOnlyExits1(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath, "-save-session", "-result-only"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "-save-session 与 -result-only 互斥") {
		t.Errorf("missing mutex error: %s", out)
	}
}

// -save-session 新建会话：stdout 首条事件为 session（type/provider/id），
// 且 jsonl 首行 metadata 的 provider 与之同值（输出契约与落盘文件同构）。
func TestCLI_SaveSessionEmitsSessionEventFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "提问", []string{"-config", cfgPath, "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		t.Fatal("stdout 无输出")
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("首行非 JSON: %s err=%v", lines[0], err)
	}
	if first["type"] != "session" {
		t.Errorf("首行 type = %v, want session（session 事件须在 Run 之前 emit）", first["type"])
	}
	if first["provider"] != "p" {
		t.Errorf("provider = %v, want p（config 的 provider name）", first["provider"])
	}
	if first["id"] == nil || first["id"] == "" {
		t.Errorf("id 缺失或空: %v", first["id"])
	}
	// session 文件应已落盘，首行 metadata 与 stdout session 事件 provider 同值。
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("session 文件数 = %d, want 1（out=%s）", len(matches), out)
	}
	data, _ := os.ReadFile(matches[0])
	firstFileLine := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
	var meta map[string]any
	if err := json.Unmarshal([]byte(firstFileLine), &meta); err != nil {
		t.Fatalf("jsonl 首行非 JSON: %s", firstFileLine)
	}
	if meta["provider"] != first["provider"] {
		t.Errorf("jsonl 首行 provider = %v, 与 stdout session 事件 %v 不一致", meta["provider"], first["provider"])
	}
	if meta["id"] != first["id"] {
		t.Errorf("jsonl 首行 id = %v, 与 stdout session 事件 %v 不一致", meta["id"], first["id"])
	}
}

// Run 出错（LLM 500）时仍落盘本轮已执行部分：救回 user prompt + 已执行的 assistant/tool 序列，
// 且配对完整可被 LoadSession 重载（resume 不中断）。验证 saveSession 在出错路径生效——
// 旧行为是 err 直接 os.Exit(1) 跳过落盘，留下「工具已执行、jsonl 未追加」的孤儿不一致。
func TestCLI_ErrorRunSavesPartialSession(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// 第 1 次：返回 tool_call（指向不存在的工具）→ handleToolCalls 回填「未知工具」结果并 append 进历史。
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"检索","tool_calls":[{"id":"c1","type":"function","function":{"name":"no_such_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		// 第 2 次起 500：非 context-length，OnLLMError 不重试，直接上抛 → Run 返 err → main exit 1（但已落盘）。
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"upstream boom"}}`)
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "救命提问", []string{"-config", cfgPath, "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Fatalf("code = %d, want 1（LLM 500 应 exit 1）; out=%s", code, out)
	}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 session file, got %v (err=%v) out=%s", matches, err, out)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"role":"user"`, `"role":"assistant"`, `"role":"tool"`, "no_such_tool", "救命提问"} {
		if !strings.Contains(s, want) {
			t.Errorf("session 缺少 %q（出错轮应救回已执行部分）: %s", want, s)
		}
	}
	// 落盘的 session 须配对完整、可被 LoadSession 重载——否则下次 resume 直接报错，等于没救。
	if _, _, err := session.LoadSession(matches[0]); err != nil {
		t.Errorf("出错落盘的 session 无法被 LoadSession 重载（配对断裂？）: %v\n%s", err, s)
	}
}

// Canceled（SIGINT→exit 130）也落盘：救回 user prompt 供下次续聊。
// Run 经 defer 在 Canceled 路径仍带回 NewMessages（含循环前 append 的 user prompt），配对完整。
func TestCLI_CanceledRunSavesSession(t *testing.T) {
	var hitOnce sync.Once
	serverHit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOnce.Do(func() { close(serverHit) }) // 子进程已进入 Run 并发出 HTTP
		time.Sleep(5 * time.Second)             // 慢响应让 SIGINT 在响应前到达
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	cmd := exec.CommandContext(ctx, os.Args[0], "-config", cfgPath, "-save-session")
	cmd.Stdin = strings.NewReader("中途取消的提问")
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
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("Canceled 应落盘 1 个 session 文件, got %d (out=%s)", len(matches), out.String())
	}
	data, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(data), `"role":"user"`) || !strings.Contains(string(data), "中途取消的提问") {
		t.Errorf("Canceled 应救回 user prompt: %s", data)
	}
}

// 无 -save-session/-session（sessPath=""）时出错不落盘：saveSession 的 sessPath 守卫——
// 出错路径不误创建 session 文件（落盘须显式 -save-session/-session 激活）。
// 注：len(NewMessages)==0 守卫是纯防御——user prompt 在 Run 循环前 append 进 newMsgs，
// defer 保证带回，故该分支在当前架构不可触发，不单测。
func TestCLI_ErrorRunNoSessionSkipsSave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, _ := runMainBin(t, "提问", []string{"-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1（500 出错）", code)
	}
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 0 {
		t.Errorf("无 -save-session/-session 时不应落盘, got %v", matches)
	}
}
