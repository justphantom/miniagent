package miniagent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
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
	var tcs []string
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","tool_calls":[%s]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}

func TestRun_TextOnlyReturnsImmediately(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("hello world")}}
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: tr}}
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
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: tr}}
	var signals []Signal
	emit := func(s Signal) { signals = append(signals, s) }
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
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{}, "p1", "x", nil, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if len(res.History) < 3 {
		t.Fatalf("history too short: %+v", res.History)
	}
	if !strings.Contains(res.History[2].Content, "未知工具") {
		t.Errorf("history[2] = %q", res.History[2].Content)
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
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: tr}}
	var signals []Signal
	emit := func(s Signal) { signals = append(signals, s) }
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
	if !resultSig.IsError || !strings.Contains(resultSig.Output, "panic") {
		t.Errorf("tool_result = %+v", resultSig)
	}
}

func TestRun_EmptyToolCallIDSynthesized(t *testing.T) {
	tool := Tool{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "", Name: "echo", Args: ""}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: tr}}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "p1", "x", nil, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	var assistantID, toolMsgID string
	for _, m := range res.History {
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

func TestRun_LLMErrorPropagates(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusServiceUnavailable}}
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: tr}}
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
	llm := &HTTPClient{APIKey: "sk", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	_, err := Run(ctx, llm, LoopConfig{}, "p1", "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
