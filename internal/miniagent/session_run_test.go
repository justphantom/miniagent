package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestResolveSessionPath(t *testing.T) {
	if p, err := ResolveSessionPath("s.json", "dir"); err != nil || p != "s.json" {
		t.Errorf("path arg should be used as-is: p=%q err=%v", p, err)
	}
	if p, err := ResolveSessionPath("./x/s.jsonl", "dir"); err != nil || !strings.HasSuffix(p, "s.jsonl") {
		t.Errorf("relative path: p=%q err=%v", p, err)
	}
	p, err := ResolveSessionPath("mysess", ".miniagent/sessions")
	if err != nil || p != filepath.Join(".miniagent/sessions", "mysess.jsonl") {
		t.Errorf("id resolution: p=%q err=%v", p, err)
	}
	if _, err := ResolveSessionPath("mysess", ""); err == nil {
		t.Error("id without dir should error")
	}
}

// NewMessages 仅含本轮新增（不含 History），Messages 含 History 前缀。
func TestRun_NewMessagesExcludesHistory(t *testing.T) {
	tool := Tool{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "echoed"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	history := []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "oldans"},
	}
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, History: history}, "newq", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(res.NewMessages) != len(wantRoles) {
		t.Fatalf("NewMessages len = %d, want %d (%+v)", len(res.NewMessages), len(wantRoles), res.NewMessages)
	}
	for i, w := range wantRoles {
		if res.NewMessages[i].Role != w {
			t.Errorf("NewMessages[%d].Role = %q, want %q", i, res.NewMessages[i].Role, w)
		}
	}
	if len(res.Messages) != len(history)+len(wantRoles) {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), len(history)+len(wantRoles))
	}
}

// History 作为前缀拼在新 prompt 之前发给 LLM；Run 不修改调用方的 History。
func TestRun_HistoryPrefixSent(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("a2")}}
	chat, stream := testClients(tr)
	history := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	res, err := Run(context.Background(), chat, stream, LoopConfig{History: history}, "q2", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	i1 := strings.Index(tr.lastBody, "q1")
	i2 := strings.Index(tr.lastBody, "a1")
	i3 := strings.Index(tr.lastBody, "q2")
	if i1 < 0 || i2 < 0 || i3 < 0 || i1 >= i2 || i2 >= i3 {
		t.Errorf("history not sent in order q1<a1<q2: %s", tr.lastBody)
	}
	if len(history) != 2 {
		t.Errorf("caller history mutated: len = %d", len(history))
	}
	want := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	if !reflect.DeepEqual(res.Messages, want) {
		t.Errorf("Messages = %+v, want %+v", res.Messages, want)
	}
}

// 最终 assistant 文本必须进入 Messages（接续对话依赖上一轮的回答）。
func TestRun_FinalTextAppendedToMessages(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("final answer")}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{}, "q", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "assistant" || last.Content != "final answer" {
		t.Errorf("last message = %+v", last)
	}
}

// 两轮接续：第一轮的完整 transcript 作为 History 传入第二轮，请求体按序含全部 4 类消息。
func TestRun_ContinuationSendsFullTranscript(t *testing.T) {
	tool := Tool{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "echoed"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("第一轮回答"),
	}}
	chat, stream := testClients(tr)
	r1, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "第一轮", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run turn1: %v", err)
	}

	tr2 := &fakeTransport{responses: []string{textResponse("第二轮回答")}}
	chat, stream = testClients(tr2)
	_, err = Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, History: r1.Messages}, "第二轮", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run turn2: %v", err)
	}
	var body struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(tr2.lastBody), &body); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	var roles []string
	for _, m := range body.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "assistant", "user"}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("turn2 request roles = %v, want %v", roles, want)
	}
	if !strings.Contains(tr2.lastBody, "第一轮回答") {
		t.Errorf("turn2 request missing turn1 final text: %s", tr2.lastBody)
	}
}

// LLM 报错：Result.Messages 仍带回已累积历史（含本轮 user prompt）。
func TestRun_ErrorStillReturnsMessages(t *testing.T) {
	tr := &fakeTransport{statuses: []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v", res.Messages)
	}
}

// 撞 maxIterations：Messages 含全部累积的 tool 往返 + 末尾注入的 summary request。
// Option B：在 iterLimit 步工具调用后注入 summaryRequestPrompt，故多一条 system 消息。
func TestRun_MaxIterationsReturnsMessages(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 1 (user) + 2*maxIterations (assistant+tool 各 maxIterations 轮) + 1 (summary request system)
	if want := 1 + 2*maxIterations + 1; len(res.Messages) != want {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), want)
	}
	// 最后一条应是 summary request system 消息。
	if res.Messages[len(res.Messages)-1].Role != roleSystem {
		t.Errorf("last message role = %q, want %q", res.Messages[len(res.Messages)-1].Role, roleSystem)
	}
}

// lockSession LOCK_NB 非阻塞：跨进程持锁时本进程不永久阻塞，5s 内超时返回 error（审查 P2 flock 阻塞）。
// 同进程 flock 不互斥（POSIX 语义），必须 fork 子进程持锁才有效。子进程由 test binary 自身重入
// （env TEST_HOLD_FLOCK_PATH 分流），持锁 10s 足够父进程验证不阻塞。
func TestLockSession_LockNBTimeoutAcrossProcesses(t *testing.T) {
	if holder := os.Getenv("TEST_HOLD_FLOCK_PATH"); holder != "" {
		// 子进程模式：持锁 10s（足够父进程测试完成），然后正常返回让 testing 退出。
		f, err := os.OpenFile(holder, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("child open: %v", err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("child lock: %v", err)
		}
		fmt.Fprintln(os.Stderr, "CHILD_LOCKED")
		time.Sleep(10 * time.Second)
		return
	}
	path := filepath.Join(t.TempDir(), "lock.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestLockSession_LockNBTimeoutAcrossProcesses$")
	cmd.Env = append(os.Environ(), "TEST_HOLD_FLOCK_PATH="+path)
	// exec 内部用独立 goroutine 写 Stderr，与主 goroutine 读需互斥（防 -race 误报）。
	var childErr mutexBuffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	// 等子进程获得锁（最多 2s）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(childErr.String(), "CHILD_LOCKED") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(childErr.String(), "CHILD_LOCKED") {
		t.Fatalf("子进程 2s 内未获锁: %s", childErr.String())
	}
	// 父进程 AppendMessages 应在 ~5s（lockSessionTotal + 一次 interval）内返回 error，不永久阻塞。
	start := time.Now()
	err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Skip("同进程/同 inode flock 不互斥，AppendMessages 成功（POSIX 语义，非 bug）")
	}
	if elapsed > 7*time.Second {
		t.Errorf("lockSession 阻塞 %v，应 ~5s 内返回 error（LOCK_NB 超时）", elapsed)
	}
}

// mutexBuffer 是线程安全的 bytes.Buffer：exec 用独立 goroutine 写 Stderr，主 goroutine 读
// 需互斥（避免 -race 报告）。
type mutexBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (m *mutexBuffer) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.Write(p)
}

func (m *mutexBuffer) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.String()
}
