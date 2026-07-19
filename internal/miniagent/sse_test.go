package miniagent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSSEStream_ContentUsageFinish(t *testing.T) {
	flow := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":", "}}]}`,
		`data: {"choices":[{"delta":{"content":"world!"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var got strings.Builder
	accum, err := parseSSEStream(strings.NewReader(flow), func(s string) error {
		got.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp := accum.response()
	if resp.Text != "Hello, world!" {
		t.Errorf("Text = %q", resp.Text)
	}
	if got.String() != "Hello, world!" {
		t.Errorf("onText = %q", got.String())
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestParseSSEStream_ToolCallAccum(t *testing.T) {
	// tool_calls 跨多个 chunk：首 chunk 带 id/name，后续只带 arguments 片段。
	flow := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	accum, err := parseSSEStream(strings.NewReader(flow), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp := accum.response()
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "read_file" || tc.Args != `{"path":"a"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
}

// onText 返回 error 必须立即中止，不再处理后续 chunk。
func TestParseSSEStream_OnTextErrorAborts(t *testing.T) {
	flow := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"a"}}]}`,
		`data: {"choices":[{"delta":{"content":"b"}}]}`,
		"",
	}, "\n")
	stopErr := errors.New("downstream closed")
	_, err := parseSSEStream(strings.NewReader(flow), func(string) error { return stopErr })
	if !errors.Is(err, stopErr) {
		t.Errorf("err = %v, want stopErr", err)
	}
}

func TestBuildChatBody_StreamOption(t *testing.T) {
	body, err := buildChatBody(Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(body)
	if !contains(s, `"stream":true`) {
		t.Errorf("missing stream:true: %s", s)
	}
	if !contains(s, `"include_usage":true`) {
		t.Errorf("missing include_usage: %s", s)
	}
}

func TestHTTPClient_DoStream_ReturnsResponse(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	var got strings.Builder
	resp, err := c.DoStream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}, func(s string) error {
		got.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "Hello" || got.String() != "Hello" {
		t.Errorf("Text=%q onText=%q", resp.Text, got.String())
	}
	if resp.Usage.InputTokens != 2 || resp.FinishReason != "stop" {
		t.Errorf("Usage=%+v FinishReason=%q", resp.Usage, resp.FinishReason)
	}
}

// 首字节前的 5xx 应重试，进入流读后不再重试（与 Do 边界一致）。
func TestHTTPClient_DoStream_RetriesBeforeFirstByte(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.DoStream(context.Background(), Request{}, nil)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}
