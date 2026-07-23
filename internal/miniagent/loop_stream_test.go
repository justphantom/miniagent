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

// textResponse / toolResponse 构造 SSE 流（恒流式下 fakeTransport 必须返回 SSE 帧）。
func textResponse(text string) string {
	return strings.Join([]string{
		fmt.Sprintf(`data: {"choices":[{"delta":{"content":%q}}]}`, text),
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: [DONE]`,
		"",
	}, "\n")
}

func toolResponse(calls ...ToolCall) string {
	chunks := make([]string, 0, len(calls)+3)
	// 首帧带 id/name 与空 arguments；OpenAI 流式协议要求 index 区分多个 tool_call。
	for i, c := range calls {
		chunks = append(chunks, fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, i, c.ID, c.Name, c.Args))
	}
	chunks = append(chunks,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: [DONE]`,
		"",
	)
	return strings.Join(chunks, "\n")
}

// 已取消的 context 必须立即中止流式 Run，避免继续烧 token。
func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	_, err := Run(ctx, llm, LoopConfig{}, "p1", "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
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
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m"}, "p1", "hi", nil, emit, nil)
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
