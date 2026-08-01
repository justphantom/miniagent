package miniagent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

// 损坏 JSON 行必须报错。
func TestLoadSession_CorruptJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	writeLines(t, path, `{"type":"session","id":"s"}`, `{not json`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for corrupt JSON line")
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
