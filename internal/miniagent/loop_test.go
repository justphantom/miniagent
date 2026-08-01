package miniagent

import (
	"context"
	"errors"
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", System: "be brief"}, "hi", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var uses []string
	onToolUse := func(name, input string) error { uses = append(uses, name); return nil }
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{OnToolUse: onToolUse}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "x", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, err := Run(context.Background(), llm, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if tr.calls != 1+maxRetries {
		t.Errorf("calls = %d, want %d (1 + maxRetries)", tr.calls, 1+maxRetries)
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "hi", LoopHooks{}, nil); err == nil {
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
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

// MaxIterations 可覆盖默认上限：设为 3 则第 3 步撞顶（不受包默认 20 影响）。
func TestRun_MaxIterationsOverride(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, 10)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 3}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Finish != finishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, finishMaxIterations)
	}
}

// MaxIterations<=0 回退默认值：第 2 步给最终文本，验证未被误解析为极小上限。
func TestRun_MaxIterationsNonPositiveUsesDefault(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	for _, v := range []int{0, -1} {
		tr := &fakeTransport{responses: []string{
			toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
			textResponse("ok"),
		}}
		llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
		res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: v}, "x", LoopHooks{}, nil)
		if err != nil {
			t.Fatalf("MaxIterations=%d: %v", v, err)
		}
		if res.Text != "ok" {
			t.Errorf("MaxIterations=%d: Text=%q want ok", v, res.Text)
		}
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
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
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

// trimForHistory：显式 limit 装裁到该值；limit<=0 用默认 maxToolResultInHistory。
func TestTrimForHistory_PerLimit(t *testing.T) {
	big := strings.Repeat("x", 10000)
	got := trimForHistory(big, 8000)
	if len(got) <= 8000 || !strings.Contains(got, "截断") {
		t.Errorf("limit=8000: len=%d, marker missing: %q", len(got), got[:min(len(got), 40)])
	}
	got0 := trimForHistory(big, 0)
	// 默认裁到 maxToolResultInHistory：长度应略大于该值（含 marker），远小于 8000。
	if len(got0) <= maxToolResultInHistory || len(got0) >= 8000 {
		t.Errorf("limit=0: len=%d, want in (%d, 8000)", len(got0), maxToolResultInHistory)
	}
}

// MaxTotalTokens：累计 token 超限即以 ErrBudgetExceeded 终止（走 error 路径）。
func TestRun_BudgetExceeded(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	bigUsage := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{bigUsage, bigUsage}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxTotalTokens: 1000}, "x", LoopHooks{}, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1（超限的那次调用计入）", res.Steps)
	}
}

// MaxTotalTokens<=0 不限：正常多步完成。
func TestRun_BudgetZeroUnlimited(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("ok"),
	}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxTotalTokens: 0}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
}

// Tool.ResultLimit 驱动历史裁剪：高限工具的长结果入历史时按其 limit 裁，
// 而非默认 2000。
func TestRun_ToolResultLimitUsedInHistory(t *testing.T) {
	long := strings.Repeat("y", 9000)
	tool := Tool{
		Name:        "bigread",
		ResultLimit: maxFileResultInHistory,
		Call:        func(context.Context, string) ToolResult { return ToolResult{Output: long} },
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "bigread", Args: "{}"}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role != roleTool {
			continue
		}
		if len(m.Content) > 8200 || len(m.Content) <= maxToolResultInHistory {
			t.Errorf("tool content not trimmed by ResultLimit: len=%d (want (%d, 8200])", len(m.Content), maxToolResultInHistory)
		}
		if !strings.Contains(m.Content, "截断") {
			t.Errorf("trim marker missing: len=%d", len(m.Content))
		}
	}
}

// C-3：reasoning 进入 transcript 并在下一步请求中以 reasoning_content 回灌。
func TestRun_ReasoningEntersHistory(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"think-step1","tool_calls":[{"id":"c1","type":"function","function":{"name":"q","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("done")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range res.Messages {
		if m.Role == roleAssistant && m.Reasoning == "think-step1" {
			found = true
		}
	}
	if !found {
		t.Errorf("reasoning not in transcript: %+v", res.Messages)
	}
	if !strings.Contains(tr.lastBody, "reasoning_content") || !strings.Contains(tr.lastBody, "think-step1") {
		t.Errorf("reasoning not sent back in next request: %s", tr.lastBody)
	}
}

// C-2：context 超限降级。首次 400(context_length) → 收紧历史 → 重试本步成功。
func TestRun_ContextLengthFallbackOnce(t *testing.T) {
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusOK},
		responses: []string{`{"error":{"message":"maximum context length exceeded"}}`, textResponse("recovered")},
	}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q, want recovered", res.Text)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want 2 (1 initial + 1 fallback)", tr.calls)
	}
}

// C-2：重试仍超限 → 只降级一次后上抛 ErrContextLength，不无限重试。
func TestRun_ContextLengthFallbackStillTooLong(t *testing.T) {
	body := `{"error":{"message":"maximum context length exceeded"}}`
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusBadRequest},
		responses: []string{body, body},
	}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, err := Run(context.Background(), llm, LoopConfig{}, "x", LoopHooks{}, nil)
	if !errors.Is(err, ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength", err)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want 2 (only one fallback)", tr.calls)
	}
}

// OnToolResult 在每个工具执行后触发一次，透传 name/call_id 与结果（含 IsError）。
func TestRun_OnToolResultFired(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "res"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var got []string
	hooks := LoopHooks{
		OnToolResult: func(name, callID string, r ToolResult) error {
			got = append(got, name+":"+callID+":"+r.Output)
			return nil
		},
	}
	if _, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0] != "q:c1:res" {
		t.Errorf("OnToolResult calls = %v, want [q:c1:res]", got)
	}
}

// OnToolResult 返回 error 沿链上抛终止循环（与 OnToolUse 同语义：下游不可写时停）。
func TestRun_OnToolResultErrorStops(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	stop := errors.New("downstream closed")
	hooks := LoopHooks{OnToolResult: func(string, string, ToolResult) error { return stop }}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
}

// OnToolUse 返回 ErrToolDenied：该工具被拒（回填拒绝结果）、不执行；其他工具正常。
func TestRun_ToolDeniedSkipped(t *testing.T) {
	toolA := Tool{Name: "a", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "A_ran"} }}
	toolB := Tool{Name: "b", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "B_ran"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "ca", Name: "a", Args: "{}"},
			ToolCall{ID: "cb", Name: "b", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	hooks := LoopHooks{OnToolUse: func(name, input string) error {
		if name == "a" {
			return ErrToolDenied
		}
		return nil
	}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{toolA, toolB}}, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var aOut, bOut string
	for _, m := range res.Messages {
		if m.Role != roleTool {
			continue
		}
		if m.ToolCallID == "ca" {
			aOut = m.Content
		}
		if m.ToolCallID == "cb" {
			bOut = m.Content
		}
	}
	if !strings.Contains(aOut, "拒绝") {
		t.Errorf("denied tool a should be rejected: %q", aOut)
	}
	if !strings.Contains(bOut, "B_ran") {
		t.Errorf("tool b should still run: %q", bOut)
	}
}
