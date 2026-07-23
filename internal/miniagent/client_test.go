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

func TestHTTPClient_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
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

// ListModels 应对 5xx 重试。
func TestHTTPClient_ListModels_RetriesTransient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// 超大 body 应被截断报错，不撑爆内存。
func TestHTTPClient_ListModels_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxModelsBodyBytes+1024))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected oversize error")
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

func TestParseRetryAfterHeader(t *testing.T) {
	if got := parseRetryAfterHeader("3"); got != 3*time.Second {
		t.Errorf("numeric seconds: got %v", got)
	}
	if got := parseRetryAfterHeader(""); got != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseRetryAfterHeader("0"); got != 0 {
		t.Errorf("zero: got %v", got)
	}
	// HTTP-date：未来 2 小时。
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	got := parseRetryAfterHeader(future)
	if got < 1*time.Hour || got > 3*time.Hour {
		t.Errorf("http-date: got %v", got)
	}
	// 过去的日期应返回 0。
	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfterHeader(past); got != 0 {
		t.Errorf("past date: got %v", got)
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

// 恒流式：buildChatBody 总会带 stream:true 与 stream_options.include_usage。
func TestBuildChatBody_AlwaysStream(t *testing.T) {
	body, err := buildChatBody(Request{Model: "m"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !contains(string(body), `"stream":true`) || !contains(string(body), `"include_usage":true`) {
		t.Errorf("body missing stream options: %s", body)
	}
}

// DoStream 在首个 200 前对 5xx 重试：首字节后不再重试。
func TestHTTPClient_DoStream_RetriesTransient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.DoStream(context.Background(), Request{}, nil)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
}

// 非 200 且不可重试状态码立即返回错误。
func TestHTTPClient_DoStream_NoRetryOn4xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.DoStream(context.Background(), Request{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// 空 API key 在 prepareDo 阶段就报错。
func TestHTTPClient_DoStream_EmptyAPIKey(t *testing.T) {
	c := &HTTPClient{}
	_, err := c.DoStream(context.Background(), Request{}, nil)
	if err == nil {
		t.Fatal("expected error for empty api key")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
