package miniagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 正常文本回复：解析出 content、usage、finish_reason。
func TestChatClient_Do_TextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
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
func TestChatClient_Do_ToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "read" || tc.Args != `{"path":"a"}` {
		t.Errorf("tc = %+v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
}

// 空 choices（端点内容过滤/代理故障）必须报错，而非当作"成功的空回答"。
func TestChatClient_Do_EmptyChoicesFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("err = %v", err)
	}
}

// 非 200 状态码：返回错误，body 截断到 500 字。
func TestChatClient_Do_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v", err)
	}
}

// 超大 body 应报错，不撑爆内存也不静默截断。
func TestChatClient_Do_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxChatBodyBytes+1024))
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected oversize error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v", err)
	}
}

// 空 API key：prepareDo 阶段就报错。
func TestChatClient_Do_EmptyAPIKey(t *testing.T) {
	c := &ChatClient{}
	_, err := c.Do(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error for empty api key")
	}
}

// BaseURL 缺 scheme：错误信息应提示 "http(s)://"。
func TestChatClient_Do_BaseURLMissingScheme(t *testing.T) {
	c := &ChatClient{APIKey: "sk", ChatURL: "api.example.com"}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err should hint missing scheme: %v", err)
	}
}

// BaseURL 用非 http(s) scheme（如 ftp）：必须在组请求前拒绝，而非等请求
// 失败才报错。
func TestChatClient_Do_BaseURLUnsupportedScheme(t *testing.T) {
	c := &ChatClient{APIKey: "sk", ChatURL: "ftp://example.com"}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "不支持") {
		t.Errorf("err should reject scheme: %v", err)
	}
}

// 恰好达到上限的 body 不应被误报截断。
func TestChatClient_Do_AcceptsLimitBody(t *testing.T) {
	// 构造一个合法的 JSON，content 长度填到接近 maxChatBodyBytes。
	// 用 padding 字段避免 JSON 结构本身超限。
	padding := bytes.Repeat([]byte("a"), maxChatBodyBytes-200)
	body := fmt.Sprintf(`{"choices":[{"message":{"content":"x","padding":"%s"},"finish_reason":"stop"}]}`, string(padding))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
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
		Tools:    []Tool{{Name: "read", Description: "d", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !contains(string(body), `"tools"`) || !contains(string(body), `"read"`) {
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

// ResultLimit 是内部字段，不得泄进工具 schema（buildChatBody 只序列化
// name/description/parameters）。
func TestBuildChatBody_ResultLimitNotSerialized(t *testing.T) {
	body, err := buildChatBody(Request{
		Model: "m",
		Tools: []Tool{{Name: "read", Description: "d", Parameters: map[string]any{"type": "object"}, ResultLimit: 8000}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(strings.ToLower(string(body)), "resultlimit") {
		t.Errorf("ResultLimit leaked into schema: %s", body)
	}
}

// reasoning 解析双兼容：reasoning_content 优先，缺失时回退 reasoning。
func TestParseChatResponse_Reasoning(t *testing.T) {
	resp, err := parseChatResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"ans","reasoning_content":"rc"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Reasoning != "rc" {
		t.Errorf("Reasoning = %q, want rc", resp.Reasoning)
	}
	resp2, err := parseChatResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"ans","reasoning":"r2"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp2.Reasoning != "r2" {
		t.Errorf("Reasoning = %q, want r2 (fallback)", resp2.Reasoning)
	}
}

// 400 + context_length 特征 → ErrContextLength（供上层单次历史收紧重试）。
func TestChatClient_Do_ContextLength400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"This model maximum context length is 8192 tokens"}}`)
	}))
	defer srv.Close()
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if !errors.Is(err, ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength", err)
	}
}

// P3-5：c.HTTP==nil 时 client() 缓存同一 *http.Client（首次 defaultTimeout 固化，后续忽略）；
// 注入 HTTP 时沿用注入值。
func TestChatClient_DefaultClientCached(t *testing.T) {
	c := &ChatClient{APIKey: "sk", ChatURL: "http://x"}
	a := c.client(2 * time.Second)
	b := c.client(5 * time.Second)
	if a != b {
		t.Error("default client not cached across calls")
	}
	if a.Timeout != 2*time.Second {
		t.Errorf("Timeout = %v, want first-call 2s (cached value preserved)", a.Timeout)
	}
	inj := &http.Client{Timeout: 30 * time.Second}
	c2 := &ChatClient{APIKey: "sk", ChatURL: "http://x", HTTP: inj}
	if c2.client(time.Second) != inj {
		t.Error("injected HTTP client not used")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
