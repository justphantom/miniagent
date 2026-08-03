package miniagent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport 把预设的非流式 JSON body 按调用顺序回放，便于 loop 测试
// 不依赖真实端点。lastBody 记录最后一次请求体；bodies 记录全部请求体供多步断言。
type fakeTransport struct {
	responses []string
	statuses  []int
	calls     int
	lastBody  string
	bodies    []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.lastBody = string(b)
		f.bodies = append(f.bodies, string(b))
		_ = req.Body.Close()
	}
	idx := f.calls
	f.calls++
	status := http.StatusOK
	if idx < len(f.statuses) {
		status = f.statuses[idx]
	}
	body := ""
	if idx < len(f.responses) {
		body = f.responses[idx]
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// textResponse 构造非流式 chat completions JSON：单条 choice，纯文本回复。
func textResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// toolResponse 构造非流式 chat completions JSON：单条 choice 带 tool_calls。
func toolResponse(calls ...ToolCall) string {
	tcs := make([]string, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}

func TestRun_TextOnlyReturnsImmediately(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("hello world")}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Model: "m", System: "be brief"}, "hi", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d", tr.calls)
	}
	if !strings.Contains(tr.lastBody, `"role":"system"`) || !strings.Contains(tr.lastBody, "be brief") {
		t.Errorf("system not sent: %s", tr.lastBody)
	}
}

func TestRun_ReActToolThenText(t *testing.T) {
	called := false
	tool := Tool{
		Name: "echo",
		Call: func(_ context.Context, args string) ToolResult {
			called = true
			return ToolResult{Output: "echoed " + args}
		},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	var uses []string
	onToolUse := func(name, input string) error { uses = append(uses, name); return nil }
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{OnToolUse: onToolUse}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if !called {
		t.Error("tool not called")
	}
	// 只通知一次 tool_use（终态文本由 Result 携带）。
	if len(uses) != 1 || uses[0] != "echo" {
		t.Errorf("uses = %v", uses)
	}
}

func TestRun_UnknownToolYieldsErrorResult(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "missing", Args: "{}"}),
		textResponse("ok"),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestRun_ToolPanicRecovered(t *testing.T) {
	tool := Tool{
		Name: "boom",
		Call: func(context.Context, string) ToolResult { panic("boom") },
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "boom", Args: "{}"}),
		textResponse("recovered"),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
}

func TestRun_LLMErrorPropagates(t *testing.T) {
	// 3 次 503：重试 maxRetries 次后仍失败，最终把错误上抛。
	tr := &fakeTransport{statuses: []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}}
	chat, stream := testClients(tr)
	_, err := Run(context.Background(), chat, stream, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if tr.calls != 1+maxRetries {
		t.Errorf("calls = %d, want %d (1 + maxRetries)", tr.calls, 1+maxRetries)
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, nil, LoopConfig{}, "hi", LoopHooks{}, nil); err == nil {
		t.Fatal("expected error")
	}
}

// 一步内的多个 tool_call 并发用例见 loop_concurrency_test.go。

// 多步 ReAct：第一步工具结果回灌，第二步拿到最终文本。
func TestRun_MultiStepReAct(t *testing.T) {
	tool := Tool{Name: "query", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "data-42"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "query", Args: "{}"}),
		toolResponse(ToolCall{ID: "c2", Name: "query", Args: "{}"}),
		textResponse("final answer"),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Text != "final answer" {
		t.Errorf("Text = %q", res.Text)
	}
}

// P2-2：端点不支持 thinking 时，跨步仅首步降级一次；后续步直接走无 thinking，不再撞 400。
func TestRun_ThinkingDowngradePersistsAcrossSteps(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	thinkErr := `{"error":{"message":"unknown parameter: reasoning_effort"}}`
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusOK, http.StatusOK},
		responses: []string{thinkErr, toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}), textResponse("done")},
	}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Model: "m", ThinkingLevel: "medium", Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	// 无降级固化时 step2 会重发 thinking → 多一次 400 + 重试（共 4 次）；固化后共 3 次。
	if len(tr.bodies) != 3 {
		t.Fatalf("calls = %d, want 3 (降级仅首步一次，step2 应直发无 thinking)", len(tr.bodies))
	}
	for i, b := range tr.bodies {
		has := strings.Contains(b, "reasoning_effort")
		if i == 0 && !has {
			t.Errorf("body[%d] 应带 thinking（首次探测）: %s", i, b)
		}
		if i != 0 && has {
			t.Errorf("body[%d] 不应带 thinking（降级应已固化）: %s", i, b)
		}
	}
	// Fix 5：降级发生 → result.ThinkingDowngraded=true（交互层据此清 baseCfg，审查 P2 跨轮固化）。
	if !res.ThinkingDowngraded {
		t.Errorf("ThinkingDowngraded should be true after a downgrade occurred")
	}
}
