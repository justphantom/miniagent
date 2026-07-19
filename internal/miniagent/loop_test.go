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

// fakeTransport queues raw HTTP response bodies for an HTTPClient.
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

func textResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

func toolResponse(calls ...ToolCall) string {
	tcs := make([]string, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","tool_calls":[%s]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}

func TestRun_TextOnlyReturnsImmediately(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("hello world")}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", System: "be brief"}, "p1", "hi", nil, nil, nil)
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
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "p1", "x", nil, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if !called {
		t.Error("tool not called")
	}
	if len(signals) != 2 {
		t.Errorf("signals = %d", len(signals))
	}
}

func TestRun_UnknownToolYieldsErrorResult(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "missing", Args: "{}"}),
		textResponse("ok"),
	}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "p1", "x", nil, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if len(res.NewMessages) < 3 {
		t.Fatalf("history too short: %+v", res.NewMessages)
	}
	if !strings.Contains(res.NewMessages[2].Content, "未知工具") {
		t.Errorf("history[2] = %q", res.NewMessages[2].Content)
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
	var signals []Signal
	emit := func(s Signal) error { signals = append(signals, s); return nil }
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "p1", "x", nil, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q", res.Text)
	}
	var resultSig Signal
	for _, s := range signals {
		if s.Kind == SignalToolResult {
			resultSig = s
		}
	}
	if !resultSig.IsError {
		t.Errorf("tool_result = %+v", resultSig)
	}
}

func TestRun_EmptyToolCallIDSynthesized(t *testing.T) {
	tool := Tool{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "", Name: "echo", Args: ""}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "p1", "x", nil, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	var assistantID, toolMsgID string
	for _, m := range res.NewMessages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistantID = m.ToolCalls[0].ID
		}
		if m.Role == "tool" {
			toolMsgID = m.ToolCallID
		}
	}
	if assistantID == "" || assistantID != toolMsgID {
		t.Errorf("pairing broken: %q vs %q", assistantID, toolMsgID)
	}
}

func TestRun_TrimLongHistory(t *testing.T) {
	big := strings.Repeat("a", 3000)
	history := []Message{{Role: "user", Content: big}, {Role: "assistant", Content: big}}
	tr := &fakeTransport{responses: []string{textResponse("ok")}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, err := Run(context.Background(), llm, LoopConfig{Model: "m"}, "p1", "hi", history, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(tr.lastBody, "hi") {
		t.Errorf("current prompt dropped: %s", tr.lastBody)
	}
}

func TestRun_LLMErrorPropagates(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusServiceUnavailable}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, err := Run(context.Background(), llm, LoopConfig{}, "p1", "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "p1", "hi", nil, nil, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	_, err := Run(ctx, llm, LoopConfig{}, "p1", "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_MaxIterationsReturnsIncompleteResult(t *testing.T) {
	// 工具调用永不停：每次都返回 tool_calls，触发 maxIterations 上限。
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "p1", "x", nil, nil, nil)
	if err != nil {
		t.Fatalf("expected nil error on max iterations, got %v", err)
	}
	if !res.Incomplete {
		t.Error("expected Incomplete=true")
	}
	if res.Steps != maxIterations {
		t.Errorf("Steps = %d, want %d", res.Steps, maxIterations)
	}
	if len(res.NewMessages) == 0 {
		t.Error("expected non-empty history to preserve burned tokens")
	}
	if res.Usage.InputTokens == 0 {
		t.Error("expected non-zero usage accounting")
	}
}

// Stream=true 时文本应分片经 SignalText 透传，最终聚合为完整 Response。
func TestRun_StreamEmitsTextDelta(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	tr := &fakeTransport{responses: []string{sse}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var deltas []string
	emit := func(sig Signal) error {
		if sig.Kind == SignalText {
			deltas = append(deltas, sig.Text)
		}
		return nil
	}
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", Stream: true}, "p1", "hi", nil, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "Hello" {
		t.Errorf("Text = %q, want Hello", res.Text)
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Errorf("deltas = %v", deltas)
	}
	if res.Usage.InputTokens != 2 {
		t.Errorf("Usage = %+v", res.Usage)
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
		_, _ = Run(context.Background(), llm, LoopConfig{Tools: tools}, "p1", "x", nil, nil, nil)
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

// 并行执行下，tool_use 信号与历史 tool 消息仍按 LLM 给定的 tool_call 原序排列。
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
	res, err := Run(context.Background(), llm, LoopConfig{Tools: tools}, "p1", "x", nil, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(uses) != 2 || uses[0] != "b" || uses[1] != "a" {
		t.Errorf("tool_use order = %v, want [b a]", uses)
	}
	var toolMsgs []Message
	for _, m := range res.NewMessages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("tool msgs = %d, want 2", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "1" || !strings.Contains(toolMsgs[0].Content, "B") {
		t.Errorf("tool[0] = %+v, want id=1 content~B", toolMsgs[0])
	}
	if toolMsgs[1].ToolCallID != "2" || !strings.Contains(toolMsgs[1].Content, "A") {
		t.Errorf("tool[1] = %+v, want id=2 content~A", toolMsgs[1])
	}
}
