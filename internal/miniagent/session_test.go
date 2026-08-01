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

// 保存→加载 round-trip：消息逐一相等，且加载端通过校验。
func TestSession_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	want := sampleTranscript()
	if err := SaveSession(path, want); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// session 文件不存在 → (nil, nil)，等同新会话。
func TestLoadSession_MissingFileReturnsNil(t *testing.T) {
	msgs, err := LoadSession(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || msgs != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", msgs, err)
	}
}

// 损坏文件必须报错，不静默丢弃历史。
func TestLoadSession_CorruptJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for corrupt JSON")
	}
}

func TestLoadSession_UnknownRoleFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.json")
	if err := os.WriteFile(path, []byte(`[{"role":"system","content":"x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestLoadSession_ToolMissingCallIDFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.json")
	if err := os.WriteFile(path, []byte(`[{"role":"tool","content":"x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for tool message without tool_call_id")
	}
}

// 超过 maxSessionBytes 的 session 文件必须拒绝：超大历史会撑内存并被完整
// 拼进下次请求烧 token。
func TestLoadSession_OversizedFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(path, make([]byte, maxSessionBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for oversized session file")
	}
}

// tool_calls 与 tool 消息配对断裂（手改/截断的 session）必须拒绝，
// 否则下一轮请求直接被端点 400。
func TestLoadSession_DanglingToolCallFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dangling.json")
	msgs := `[{"role":"user","content":"q"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","name":"read","args":"{}"}]}]`
	if err := os.WriteFile(path, []byte(msgs), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for dangling tool_call")
	}
}

func TestLoadSession_OrphanToolMessageFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan.json")
	msgs := `[{"role":"user","content":"q"},{"role":"tool","tool_call_id":"cX","content":"x"}]`
	if err := os.WriteFile(path, []byte(msgs), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for orphan tool message")
	}
}

// 空 transcript 拒写：nil 会落成 "null"，"空会话"与"新会话"无法区分。
func TestSaveSession_EmptyRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := SaveSession(path, nil); err == nil {
		t.Fatal("expected error for empty transcript")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not be created")
	}
}

// 对话内容属敏感数据：落盘权限必须 0o600。
func TestSaveSession_FileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perm.json")
	if err := SaveSession(path, sampleTranscript()); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

// C-3：reasoning 随 Message.Reasoning 落盘（与 Content 同级、对称），支持
// reasoning 模型跨会话续跑。
func TestSaveSession_ReasoningPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a", Reasoning: "thought chain"},
	}
	if err := SaveSession(path, msgs); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(got) != 2 || got[1].Reasoning != "thought chain" {
		t.Errorf("reasoning not persisted: %+v", got)
	}
}

// History 作为前缀拼在新 prompt 之前发给 LLM；Run 不修改调用方的 History。
func TestRun_HistoryPrefixSent(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("a2")}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "q", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "assistant" || last.Content != "final answer" {
		t.Errorf("last message = %+v", last)
	}
}

// 两轮接续：第一轮的完整 transcript（user/assistant+tool_calls/tool/assistant
// 最终文本）作为 History 传入第二轮，请求体须按序包含全部 4 类消息。
func TestRun_ContinuationSendsFullTranscript(t *testing.T) {
	tool := Tool{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "echoed"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("第一轮回答"),
	}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	r1, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "第一轮", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run turn1: %v", err)
	}

	tr2 := &fakeTransport{responses: []string{textResponse("第二轮回答")}}
	llm2 := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr2}}
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
	// 请求体 = 第一轮 transcript（4 条）+ 本轮 user；第二轮的 assistant 回答
	// 是响应，不在请求体中。
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 1 条 user + 每步 assistant+tool 各 1 条。
	if want := 1 + 2*maxIterations; len(res.Messages) != want {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), want)
	}
}
