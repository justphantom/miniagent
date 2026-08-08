package session

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/looptest"
)

// maxIterations 对齐 core 默认迭代上限（miniagent.maxIterations 未导出），供本文件撞上限断言。
const maxIterations = 20

func TestResolveSessionPath(t *testing.T) {
	// 合法 id → {dir}/{id}.jsonl
	p, err := ResolveSessionPath("mysess", ".miniagent/sessions")
	if err != nil || p != filepath.Join(".miniagent/sessions", "mysess.jsonl") {
		t.Errorf("id resolution: p=%q err=%v", p, err)
	}
	// 生成 id 形态（字母/数字/-）合法。
	if _, err := ResolveSessionPath("20260805-143022-abc123", "dir"); err != nil {
		t.Errorf("generated-style id should be valid: %v", err)
	}
	// 非法字符（路径分隔符、点、空格等）必须报错。
	for _, bad := range []string{"s.json", "./x/s", "a/b", "a\\b", "a b", ".."} {
		if _, err := ResolveSessionPath(bad, "dir"); err == nil {
			t.Errorf("bad id %q should error", bad)
		}
	}
	// 空报错。
	if _, err := ResolveSessionPath("", "dir"); err == nil {
		t.Error("empty id should error")
	}
	// dir 空报错。
	if _, err := ResolveSessionPath("mysess", ""); err == nil {
		t.Error("id without dir should error")
	}
}

// NewMessages 仅含本轮新增（不含 History），Messages 含 History 前缀。
func TestRun_NewMessagesExcludesHistory(t *testing.T) {
	tool := miniagent.Tool{Name: "echo", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "echoed"} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		looptest.TextResponse("done"),
	}}
	llm := looptest.NewFakeLLM(tr)
	history := []miniagent.Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "oldans"},
	}
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: history}, "newq", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
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

// History 作为前缀拼在新 prompt 之前发给 LLM；miniagent.Run 不修改调用方的 History。
func TestRun_HistoryPrefixSent(t *testing.T) {
	tr := &looptest.FakeTransport{Responses: []string{looptest.TextResponse("a2")}}
	llm := looptest.NewFakeLLM(tr)
	history := []miniagent.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{History: history}, "q2", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	i1 := strings.Index(tr.LastBody, "q1")
	i2 := strings.Index(tr.LastBody, "a1")
	i3 := strings.Index(tr.LastBody, "q2")
	if i1 < 0 || i2 < 0 || i3 < 0 || i1 >= i2 || i2 >= i3 {
		t.Errorf("history not sent in order q1<a1<q2: %s", tr.LastBody)
	}
	if len(history) != 2 {
		t.Errorf("caller history mutated: len = %d", len(history))
	}
	want := []miniagent.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	if len(res.Messages) != len(want) {
		t.Fatalf("Messages len = %d, want %d: %+v", len(res.Messages), len(want), res.Messages)
	}
	// §P0-B 后新产生的 assistant 带 miniagent.Usage/Ts、新 user 带 Ts（非确定性），故只校验 Role+Content，
	// 不再整体 reflect.DeepEqual（history q1/a1 仍无 miniagent.Usage/Ts，深比会因新字段失稳）。
	for i, w := range want {
		if res.Messages[i].Role != w.Role || res.Messages[i].Content != w.Content {
			t.Errorf("Messages[%d] = {Role:%q Content:%q}, want {Role:%q Content:%q}",
				i, res.Messages[i].Role, res.Messages[i].Content, w.Role, w.Content)
		}
	}
}

// 最终 assistant 文本必须进入 Messages（接续对话依赖上一轮的回答）。
func TestRun_FinalTextAppendedToMessages(t *testing.T) {
	tr := &looptest.FakeTransport{Responses: []string{looptest.TextResponse("final answer")}}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{}, "q", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "assistant" || last.Content != "final answer" {
		t.Errorf("last message = %+v", last)
	}
}

// 两轮接续：第一轮的完整 transcript 作为 History 传入第二轮，请求体按序含全部 4 类消息。
func TestRun_ContinuationSendsFullTranscript(t *testing.T) {
	tool := miniagent.Tool{Name: "echo", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "echoed"} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		looptest.TextResponse("第一轮回答"),
	}}
	llm := looptest.NewFakeLLM(tr)
	r1, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}}, "第一轮", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run turn1: %v", err)
	}

	tr2 := &looptest.FakeTransport{Responses: []string{looptest.TextResponse("第二轮回答")}}
	llm = looptest.NewFakeLLM(tr2)
	_, err = miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: r1.Messages}, "第二轮", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run turn2: %v", err)
	}
	var body struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(tr2.LastBody), &body); err != nil {
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
	if !strings.Contains(tr2.LastBody, "第一轮回答") {
		t.Errorf("turn2 request missing turn1 final text: %s", tr2.LastBody)
	}
}

// LLM 报错：Result.Messages 仍带回已累积历史（含本轮 user prompt）。
func TestRun_ErrorStillReturnsMessages(t *testing.T) {
	tr := &looptest.FakeTransport{Statuses: []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{}, "hi", miniagent.LoopHooks{}, nil)
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
	tool := miniagent.Tool{Name: "loop", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = looptest.ToolResponse(miniagent.ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &looptest.FakeTransport{Responses: responses}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}}, "x", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	// 1 (user) + 2*maxIterations (assistant+tool 各 maxIterations 轮)；summary 引导消息不进 transcript。
	if want := 1 + 2*maxIterations; len(res.Messages) != want {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), want)
	}
	// 最后一条应是最近一次工具结果（summary 经临时 reqMsgs 发送，不污染 Messages）。
	if res.Messages[len(res.Messages)-1].Role != miniagent.RoleTool {
		t.Errorf("last message role = %q, want %q", res.Messages[len(res.Messages)-1].Role, miniagent.RoleTool)
	}
}
