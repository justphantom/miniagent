package miniagent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
	var signals []Signal
	emit := func(s Signal) error { signals = append(signals, s); return nil }
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if !called {
		t.Error("tool not called")
	}
	// 只 emit tool_use（文本不再增量透传，终态由 Result 携带）。
	if len(signals) != 1 || signals[0].Kind != SignalToolUse || signals[0].Name != "echo" {
		t.Errorf("signals = %+v", signals)
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
	// 单次 503：无重试，立即返回 error。
	tr := &fakeTransport{statuses: []int{http.StatusServiceUnavailable}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, err := Run(context.Background(), llm, LoopConfig{}, "hi", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", tr.calls)
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "hi", nil, nil); err == nil {
		t.Fatal("expected error")
	}
}

// 工具调用永不停：每次都返回 tool_calls，触发 maxIterations 上限。
// 终止信号由 Steps=maxIterations + 空 Text 表达。
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
	if res.Text != "" {
		t.Errorf("Text = %q, want empty (truncated)", res.Text)
	}
	if res.Usage.InputTokens == 0 {
		t.Error("expected non-zero usage accounting")
	}
}

// 一步内的多个 tool_call 必须并发执行：3 个工具都启动后才能 release 任一个，
// 串行执行下最多只有 1 个工具会在 release 前启动。
func TestRun_ToolsRunInParallel(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	mk := func(name string) Tool {
		return Tool{
			Name: name,
			Call: func(context.Context, string) ToolResult {
				started <- name
				<-release
				return ToolResult{Output: name}
			},
		}
	}
	tools := []Tool{mk("a"), mk("b"), mk("c")}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "1", Name: "a", Args: "{}"},
			ToolCall{ID: "2", Name: "b", Args: "{}"},
			ToolCall{ID: "3", Name: "c", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}

	done := make(chan struct{})
	go func() {
		_, _ = Run(context.Background(), llm, LoopConfig{Tools: tools}, "x", nil, nil)
		close(done)
	}()

	got := make(map[string]bool, 3)
	for range 3 {
		select {
		case name := <-started:
			got[name] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d tools started before release, expected 3 (got %v)", len(got), got)
		}
	}
	close(release)
	<-done
}

// 并行执行下，tool_use 信号仍按 LLM 给定的 tool_call 原序排列。
func TestRun_ParallelToolResultsMatchOrder(t *testing.T) {
	tools := []Tool{
		{Name: "a", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "A"} }},
		{Name: "b", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "B"} }},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "1", Name: "b", Args: "{}"},
			ToolCall{ID: "2", Name: "a", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var uses []string
	emit := func(s Signal) error {
		if s.Kind == SignalToolUse {
			uses = append(uses, s.Name)
		}
		return nil
	}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: tools}, "x", emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(uses) != 2 || uses[0] != "b" || uses[1] != "a" {
		t.Errorf("tool_use order = %v, want [b a]", uses)
	}
}

// 已取消的 context 必须立即中止 Run，避免继续烧 token。
func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	_, err := Run(ctx, llm, LoopConfig{}, "hi", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

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
