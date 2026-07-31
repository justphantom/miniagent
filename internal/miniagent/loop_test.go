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
// 不依赖真实端点。lastBody 记录最后一次请求体供断言。
type fakeTransport struct {
	responses []string
	statuses  []int
	calls     int
	lastBody  string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.lastBody = string(b)
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", System: "be brief"}, "hi", nil, nil)
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var uses []string
	onToolUse := func(name, input string) error { uses = append(uses, name); return nil }
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", onToolUse, nil)
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "x", nil, nil)
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", nil, nil)
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, err := Run(context.Background(), llm, LoopConfig{}, "hi", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if tr.calls != 1+maxRetries {
		t.Errorf("calls = %d, want %d (1 + maxRetries)", tr.calls, 1+maxRetries)
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "hi", nil, nil); err == nil {
		t.Fatal("expected error")
	}
}

// 工具调用永不停：每次都返回 tool_calls，触发 maxIterations 上限。
// 终止信号由 Finish=max_iterations 表达（Steps=maxIterations + 空 Text）。
func TestRun_MaxIterationsReturnsBurnedUsage(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", nil, nil)
	if err != nil {
		t.Fatalf("expected nil error on max iterations, got %v", err)
	}
	if res.Steps != maxIterations {
		t.Errorf("Steps = %d, want %d", res.Steps, maxIterations)
	}
	if res.Finish != finishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, finishMaxIterations)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty (truncated)", res.Text)
	}
	if res.Usage.InputTokens == 0 {
		t.Error("expected non-zero usage accounting")
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
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", nil, nil)
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
