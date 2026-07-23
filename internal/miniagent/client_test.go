package miniagent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 正常文本回复：解析出 content、usage、finish_reason。
func TestHTTPClient_Do_TextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v", resp.ToolCalls)
	}
}

// 带 tool_calls 的回复：name/arguments/id 正确解析。
func TestHTTPClient_Do_ToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "read_file" || tc.Args != `{"path":"a"}` {
		t.Errorf("tc = %+v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
}

// 非 200 状态码：返回错误，body 截断到 500 字。
func TestHTTPClient_Do_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v", err)
	}
}

// 超大 body 应报错，不撑爆内存也不静默截断。
func TestHTTPClient_Do_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxChatBodyBytes+1024))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected oversize error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v", err)
	}
}

// 空 API key：prepareDo 阶段就报错。
func TestHTTPClient_Do_EmptyAPIKey(t *testing.T) {
	c := &HTTPClient{}
	_, err := c.Do(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error for empty api key")
	}
}

// 恰好达到上限的 body 不应被误报截断。
func TestHTTPClient_Do_AcceptsLimitBody(t *testing.T) {
	// 构造一个合法的 JSON，content 长度填到接近 maxChatBodyBytes。
	// 用 padding 字段避免 JSON 结构本身超限。
	padding := bytes.Repeat([]byte("a"), maxChatBodyBytes-200)
	body := fmt.Sprintf(`{"choices":[{"message":{"content":"x","padding":"%s"},"finish_reason":"stop"}]}`, string(padding))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "x" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestBuildChatBody_IncludesTools(t *testing.T) {
	body, err := buildChatBody(Request{
		Model:    "m",
		System:   "sys",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Name: "read_file", Description: "d", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !contains(string(body), `"tools"`) || !contains(string(body), `"read_file"`) {
		t.Errorf("body missing tools: %s", body)
	}
}

func TestBuildChatBody_SkipsZeroMaxTokens(t *testing.T) {
	body, err := buildChatBody(Request{Model: "m", MaxTokens: 0})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if contains(string(body), `"max_tokens"`) {
		t.Errorf("body should not include max_tokens: %s", body)
	}
}

// 非流式：buildChatBody 不应再带 stream / stream_options。
func TestBuildChatBody_NoStream(t *testing.T) {
	body, err := buildChatBody(Request{Model: "m"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if contains(string(body), `"stream"`) || contains(string(body), `"stream_options"`) {
		t.Errorf("body should not include stream fields: %s", body)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
