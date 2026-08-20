package openai

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

	"github.com/justphantom/miniagent/miniagent"
)

// Normal text response: content, usage, and finish_reason are parsed out.
func TestChatClient_Do_TextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), miniagent.Request{Model: "m", Messages: []miniagent.Message{{Role: "user", Content: "hi"}}})
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

// Response with tool_calls: name/arguments/id are parsed correctly.
func TestChatClient_Do_ToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
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

// Empty choices (endpoint content filtering / proxy failure) must error, not be treated as a "successful empty answer".
func TestChatClient_Do_EmptyChoicesFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("err = %v", err)
	}
}

// Non-200 status code: returns an error.
func TestChatClient_Do_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v", err)
	}
}

// An oversized body should error, neither blowing up memory nor being silently truncated.
func TestChatClient_Do_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxChatBodyBytes+1024))
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected oversize error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v", err)
	}
}

// Empty API key: errors at the prepareDo stage.
func TestChatClient_Do_EmptyAPIKey(t *testing.T) {
	c := &ChatClient{}
	_, err := c.Do(context.Background(), miniagent.Request{})
	if err == nil {
		t.Fatal("expected error for empty api key")
	}
}

// BaseURL missing scheme: the error message should hint at "http(s)://".
func TestChatClient_Do_BaseURLMissingScheme(t *testing.T) {
	c := &ChatClient{APIKey: "sk", ChatURL: "api.example.com"}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err should hint missing scheme: %v", err)
	}
}

// BaseURL using a non-http(s) scheme (e.g. ftp): must be rejected before building the request,
// rather than only erroring once the request fails.
func TestChatClient_Do_BaseURLUnsupportedScheme(t *testing.T) {
	c := &ChatClient{APIKey: "sk", ChatURL: "ftp://example.com"}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("err should reject scheme: %v", err)
	}
}

// A body exactly at the limit must not be falsely flagged as truncated.
func TestChatClient_Do_AcceptsLimitBody(t *testing.T) {
	// Build a valid JSON whose content length fills up to near maxChatBodyBytes.
	// A padding field avoids pushing the JSON structure itself over the limit.
	padding := bytes.Repeat([]byte("a"), maxChatBodyBytes-200)
	body := fmt.Sprintf(`{"choices":[{"message":{"content":"x","padding":"%s"},"finish_reason":"stop"}]}`, string(padding))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "x" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestBuildChatBody_IncludesTools(t *testing.T) {
	body, err := buildChatBody(miniagent.Request{
		Model:    "m",
		System:   "sys",
		Messages: []miniagent.Message{{Role: "user", Content: "hi"}},
		Tools:    []miniagent.Tool{{Name: "read", Description: "d", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !contains(string(body), `"tools"`) || !contains(string(body), `"read"`) {
		t.Errorf("body missing tools: %s", body)
	}
}

func TestBuildChatBody_SkipsZeroMaxTokens(t *testing.T) {
	body, err := buildChatBody(miniagent.Request{Model: "m", MaxTokens: 0})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if contains(string(body), `"max_tokens"`) {
		t.Errorf("body should not include max_tokens: %s", body)
	}
}

// Non-streaming: buildChatBody must not carry stream / stream_options.
func TestBuildChatBody_NoStream(t *testing.T) {
	body, err := buildChatBody(miniagent.Request{Model: "m"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if contains(string(body), `"stream"`) || contains(string(body), `"stream_options"`) {
		t.Errorf("body should not include stream fields: %s", body)
	}
}

// ResultLimit is an internal field and must not leak into the tool schema (buildChatBody only
// serializes name/description/parameters).
func TestBuildChatBody_ResultLimitNotSerialized(t *testing.T) {
	body, err := buildChatBody(miniagent.Request{
		Model: "m",
		Tools: []miniagent.Tool{{Name: "read", Description: "d", Parameters: map[string]any{"type": "object"}, ResultLimit: 8000}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(strings.ToLower(string(body)), "resultlimit") {
		t.Errorf("ResultLimit leaked into schema: %s", body)
	}
}

// reasoning parsing is dual-compatible: reasoning_content takes precedence, falling back to reasoning when absent.
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

// 400 + context_length signature -> ErrContextLength (lets the upper layer do a single history-tightening retry).
func TestChatClient_Do_ContextLength400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"This model maximum context length is 8192 tokens"}}`)
	}))
	defer srv.Close()
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if !errors.Is(err, miniagent.ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength", err)
	}
}

// NEW-6: when c.HTTP==nil, client() is built per call with defaultTimeout (cache removed). The former
// defaultClient cache was shared by the chat(120s)/models(30s) endpoints, and sync.Once pinned the
// first caller's timeout while later values were silently ignored; now each call uses its own timeout.
func TestChatClient_DefaultClientPerCallTimeout(t *testing.T) {
	c := &ChatClient{APIKey: "sk", ChatURL: "http://x"}
	if got := c.client(2 * time.Second); got.Timeout != 2*time.Second {
		t.Errorf("first call Timeout = %v, want 2s", got.Timeout)
	}
	if got := c.client(5 * time.Second); got.Timeout != 5*time.Second {
		t.Errorf("second call Timeout = %v, want 5s (after removing the cache each call uses its own timeout, not pinned by the first call)", got.Timeout)
	}
	inj := &http.Client{Timeout: 30 * time.Second}
	c2 := &ChatClient{APIKey: "sk", ChatURL: "http://x", HTTP: inj}
	if c2.client(time.Second) != inj {
		t.Error("injected HTTP client not used")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
