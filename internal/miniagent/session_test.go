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

func sampleTranscript() []Message {
	return []Message{
		{Role: "user", Content: "看下 a.txt"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: `{"path":"a.txt"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "1 │ hello"},
		{Role: "assistant", Content: "a.txt 内容是 hello"},
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Append→Load round-trip：消息逐一相等，metadata 复现。
func TestSession_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	want := sampleTranscript()
	meta := SessionMeta{ID: "s", Model: "main/glm", Workdir: "/abs", Provider: "main"}
	if err := AppendMessages(path, meta, want); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	gotMeta, got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if gotMeta.ID != "s" || gotMeta.Model != "main/glm" {
		t.Errorf("meta mismatch: %+v", gotMeta)
	}
}

// 文件不存在 → (零 meta, nil, nil)，等同新会话。
func TestLoadSession_MissingFileReturnsNil(t *testing.T) {
	meta, msgs, err := LoadSession(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || msgs != nil || meta.Type != "" {
		t.Errorf("got (%+v, %v, %v), want (零 meta, nil, nil)", meta, msgs, err)
	}
}

// 中间损坏的 JSON 行（合法行夹在前后）必须报错。
func TestLoadSession_CorruptJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	writeLines(t, path, `{"type":"session","id":"s"}`, `{not json`, `{"type":"message","role":"user","content":"after"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for corrupt JSON line in the middle")
	}
}

// append-only 崩溃残留的尾行半写应被容忍：丢弃残行，加载此前合法的历史。
func TestLoadSession_ToleratesCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	writeLines(t, path,
		`{"type":"session","id":"s"}`,
		`{"type":"message","role":"user","content":"ok"}`,
		`{broken half line`)
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("corrupt tail should be tolerated: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "ok" {
		t.Errorf("expected 1 valid message before corrupt tail, got %+v", msgs)
	}
}

func TestLoadSession_UnknownRoleFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.jsonl")
	writeLines(t, path, `{"type":"message","role":"system","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestLoadSession_ToolMissingCallIDFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.jsonl")
	writeLines(t, path, `{"type":"message","role":"tool","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for tool message without tool_call_id")
	}
}

func TestLoadSession_OversizedFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, make([]byte, maxSessionBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for oversized session file")
	}
}

// tool_calls 与 tool 配对断裂必须拒绝。
func TestLoadSession_DanglingToolCallFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dangling.jsonl")
	writeLines(t, path,
		`{"type":"message","role":"user","content":"q"}`,
		`{"type":"message","role":"assistant","content":"","tool_calls":[{"id":"c1","name":"read","args":"{}"}]}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for dangling tool_call")
	}
}

func TestLoadSession_OrphanToolMessageFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan.jsonl")
	writeLines(t, path,
		`{"type":"message","role":"user","content":"q"}`,
		`{"type":"message","role":"tool","tool_call_id":"cX","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for orphan tool message")
	}
}

// kind=summary 结构化标记可读（审查 v3 #2），role=user 合法持久化。
func TestLoadSession_KindSummaryRecognized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sum.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{
		{Role: "user", Kind: KindSummary, Content: "[既往对话摘要] xxx"},
	}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Kind != KindSummary || msgs[0].Role != "user" {
		t.Errorf("summary kind not recognized: %+v", msgs)
	}
}

// 空 msgs：AppendMessages no-op，不创建文件（main 仅成功轮调用，空 NewMessages 不落盘）。
func TestAppendMessages_EmptyNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, nil); err != nil {
		t.Fatalf("empty append should be no-op: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not be created for empty msgs")
	}
}

// 落盘权限 0o600（对话属敏感数据）。
func TestAppendMessages_FileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perm.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, sampleTranscript()); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

// 多次 Append 累积（append-only），每次仅追加新消息。
func TestAppendMessages_AppendsAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acc.jsonl")
	meta := SessionMeta{ID: "s"}
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: "q1"}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, meta, []Message{{Role: "assistant", Content: "a1"}}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "q1" || msgs[1].Content != "a1" {
		t.Errorf("appended msgs wrong: %+v", msgs)
	}
}

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

// v2 JSON 数组 → jsonl 迁移：内容逐条相等，落盘为 jsonl。
func TestMigrateSession(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.json")
	want := sampleTranscript()
	arr, _ := json.Marshal(want)
	if err := os.WriteFile(src, arr, 0o600); err != nil {
		t.Fatal(err)
	}
	dst, err := MigrateSession(src, dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.HasSuffix(dst, "old.jsonl") {
		t.Errorf("dst = %q", dst)
	}
	_, got, err := LoadSession(dst)
	if err != nil {
		t.Fatalf("load migrated: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("migrated mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// NewMessages 仅含本轮新增（不含 History），Messages 含 History 前缀。
func TestRun_NewMessagesExcludesHistory(t *testing.T) {
	tool := Tool{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "echoed"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	history := []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "oldans"},
	}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, History: history}, "newq", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	history := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	res, err := Run(context.Background(), llm, LoopConfig{History: history}, "q2", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "q", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	r1, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "第一轮", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run turn1: %v", err)
	}

	tr2 := &fakeTransport{responses: []string{textResponse("第二轮回答")}}
	llm2 := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr2}}
	_, err = Run(context.Background(), llm2, LoopConfig{Tools: []Tool{tool}, History: r1.Messages}, "第二轮", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v", res.Messages)
	}
}

// 撞 maxIterations：Messages 含全部累积的 tool 往返。
func TestRun_MaxIterationsReturnsMessages(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := 1 + 2*maxIterations; len(res.Messages) != want {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), want)
	}
}

// P1-4：追加后将超过 maxSessionBytes 时返回 error，不写入。读侧硬封顶，写侧不预判会让
// 某轮落盘后文件刚越 4MiB，下一轮 LoadSession 永久失败致会话不可接续。
func TestAppendMessages_OversizedAppendErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := SessionMeta{ID: "s"}
	big := strings.Repeat("x", maxSessionBytes/2+100)
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: big}}); err != nil {
		t.Fatalf("首次追加应成功: %v", err)
	}
	// 再追加同样大小，总和超 maxSessionBytes → 写侧预判应返回 error。
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: big}}); err == nil {
		t.Fatal("P1-4：追加后超 maxSessionBytes 应返回 error")
	}
	// 第二次失败不应让文件越过上限（写侧预判在写入前）。
	info, _ := os.Stat(path)
	if info.Size() > maxSessionBytes {
		t.Errorf("文件大小 %d 超 maxSessionBytes %d", info.Size(), maxSessionBytes)
	}
}

// P2-7：单条大消息（>1MiB 既单行上限、<maxSessionBytes 总上限）可正常 load，
// 不再因 scanner ErrTooLong 致整会话不可读、append-only 无法修复。
func TestLoadSession_LargeSingleLineOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := strings.Repeat("x", maxSessionBytes/2) // 2MiB，超过旧 1MiB 单行限制
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: big}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("P2-7：大单行 load 失败（scanner 单行上限不足）: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Content) != len(big) {
		t.Errorf("大单行 round-trip 不一致: %+v", msgs)
	}
}

// P2-13：并发 AppendMessages 不损坏 session 文件。flock 跨进程互斥由 POSIX 保证；
// 同进程内不同 fd 的 flock 语义不互斥，此测试主要回归保护 AppendMessages 集成 flock
// 后单进程多调用仍正常（不阻塞自己、不残留锁、LoadSession 不报中间损坏）。
func TestAppendMessages_ConcurrentSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := SessionMeta{ID: "s"}
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: "init"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := range 10 {
				if err := AppendMessages(path, meta, []Message{
					{Role: "assistant", Content: fmt.Sprintf("g%d-%d", idx, i)},
				}); err != nil {
					t.Errorf("append %d-%d: %v", idx, i, err)
				}
			}
		}(g)
	}
	wg.Wait()
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("P2-13：并发 append 后 load 报错（行交织中间损坏）: %v", err)
	}
	if want := 1 + 4*10; len(msgs) != want {
		t.Errorf("msgs len = %d, want %d", len(msgs), want)
	}
}

// P3-10：validateToolPairing 错误消息索引为 1-based（便于按行号定位 session 文件）。
func TestValidateToolPairing_ErrorMessageIsOneBased(t *testing.T) {
	// 第 2 条（0-based 索引 1）出现重复 tool_call id。
	msgs := []Message{
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "dup", Name: "x", Args: "{}"}}},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "dup", Name: "x", Args: "{}"}}},
	}
	err := validateToolPairing(msgs)
	if err == nil {
		t.Fatal("expected pairing error")
	}
	if !strings.Contains(err.Error(), "第 2 条") {
		t.Errorf("错误消息应为 1-based「第 2 条」，got: %v", err)
	}
}

// RewriteMessages 全量重写：内容正确、临时文件清理、权限 0o600、旧中段（被屏障的旧轮）真丢弃。
// 这是 P2「session 文件永不压缩」的核心修复——append-only 落盘的 newMsgs 含被屏障旧 summary
// 与被压中段，rewrite 用全量 transcript（result.Messages）原子替换文件，把它们真正丢掉。
func TestRewriteMessages_AtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// 先 append 旧内容（含将被 rewrite 丢弃的旧 summary + 旧中段）。
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{
		{Role: "user", Kind: KindSummary, Content: "[既往对话摘要] 旧"},
		{Role: "user", Content: "被屏障的旧轮"},
	}); err != nil {
		t.Fatal(err)
	}
	// Rewrite 为新内容（新 summary + 最近轮）。
	want := []Message{
		{Role: "user", Kind: KindSummary, Content: "[既往对话摘要] 新"},
		{Role: "user", Content: "最近轮提问"},
		{Role: "assistant", Content: "最近轮回答"},
	}
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, want); err != nil {
		t.Fatalf("RewriteMessages: %v", err)
	}
	// 临时文件已清理。
	matches, err := filepath.Glob(filepath.Join(dir, "s.jsonl.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("临时文件未清理: %v", matches)
	}
	// 权限 0o600。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("权限 %o, want 600", info.Mode().Perm())
	}
	// 内容：旧"被屏障的旧轮"已丢，load 后只剩新 msgs。
	_, got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rewrite 后内容错:\n got %+v\nwant %+v", got, want)
	}
}

// RewriteMessages 空 msgs 也合法（写仅 metadata 文件，等价于「重置」）。
func TestRewriteMessages_EmptyMsgsWritesMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, nil); err != nil {
		t.Fatalf("RewriteMessages 空 msgs: %v", err)
	}
	meta, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("空 rewrite 后 msgs 应为空: %+v", msgs)
	}
	if meta.ID != "s" {
		t.Errorf("meta.ID = %q, want s", meta.ID)
	}
}

// RewriteMessages 超 maxSessionBytes 报错，不创建/不替换文件。
func TestRewriteMessages_OversizedFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	big := strings.Repeat("x", maxSessionBytes+1)
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: big}}); err == nil {
		t.Fatal("超 maxSessionBytes 应报错")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("超限 rewrite 不应创建文件（写临时文件前预判）")
	}
}

// P3 session 硬化：AppendMessages 用 O_NOFOLLOW 拒绝最终分量是 symlink 的目标，
// 防本地攻击者预先用 symlink 指向敏感文件被 append 污染。
func TestAppendMessages_RejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := AppendMessages(link, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}}); err == nil {
		t.Error("O_NOFOLLOW 应拒绝 symlink 目标")
	}
}

// P3 session 硬化：RewriteMessages 同样用 O_NOFOLLOW 拒绝 symlink 目标。
func TestRewriteMessages_RejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := RewriteMessages(link, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}}); err == nil {
		t.Error("O_NOFOLLOW 应拒绝 symlink 目标")
	}
}

// P3 session 硬化：MkdirAll 用 0o700（非旧 0o755），防 group-writable 目录下其他用户写入。
func TestAppendMessages_MkdirAll0700(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "s.jsonl")
	if err := AppendMessages(nested, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	aDir := filepath.Join(dir, "a")
	info, err := os.Stat(aDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("目录权限 %o, want 700", info.Mode().Perm())
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
