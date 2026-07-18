package miniagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPClient_Do_ReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing auth header")
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk-test", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "hi" {
		t.Errorf("Text = %q, want hi", resp.Text)
	}
	if resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestHTTPClient_Do_RetriesTransient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestHTTPClient_Do_NoRetryOn4xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestHTTPClient_Do_EmptyAPIKey(t *testing.T) {
	c := &HTTPClient{}
	_, err := c.Do(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error for empty api key")
	}
}

func TestHTTPClient_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0] != "a" || models[1] != "b" {
		t.Errorf("models = %v", models)
	}
}

func TestParseChatResponse_ToolCalls(t *testing.T) {
	raw := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	resp, err := parseChatResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].ID != "c1" {
		t.Errorf("tool call = %+v", resp.ToolCalls[0])
	}
}

func TestRetryableStatus(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		if !retryableStatus(code) {
			t.Errorf("%d should be retryable", code)
		}
	}
	if retryableStatus(400) || retryableStatus(404) {
		t.Error("4xx should not be retryable")
	}
}

func TestParseRetryAfter(t *testing.T) {
	got := parseRetryAfter([]byte(`{"error":{"retry_after":2.5}}`))
	if got < 2*time.Second || got > 3*time.Second {
		t.Errorf("retry_after = %v", got)
	}
	if parseRetryAfter([]byte(`{}`)) != 0 {
		t.Error("expected 0")
	}
}

func TestBuildChatBody_IncludesTools(t *testing.T) {
	body, err := buildChatBody(Request{
		Model:    "m",
		System:   "sys",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Name: "read_file", Description: "d", Parameters: map[string]any{"type": "object"}}},
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

func TestHTTPClient_Do_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
