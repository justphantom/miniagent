package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// A minimal well-formed Anthropic SSE stream returning a single text delta "hi".
const okSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"input_tokens\":5}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestStreamClient_AuthHeadersAndBody(t *testing.T) {
	var gotHeaders http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		_ = json.Unmarshal(buf[:n], &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okSSE))
	}))
	defer srv.Close()
	c, err := NewStreamClient("k-1", srv.URL, srv.Client(), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.DoStream(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi" {
		t.Errorf("Text = %q", res.Text)
	}
	if gotHeaders.Get("X-Api-Key") != "k-1" {
		t.Errorf("x-api-key = %q", gotHeaders.Get("X-Api-Key"))
	}
	if gotHeaders.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotHeaders.Get("Anthropic-Version"))
	}
	if gotHeaders.Get("Anthropic-Beta") != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q", gotHeaders.Get("Anthropic-Beta"))
	}
	if gotBody["model"] != "m" {
		t.Errorf("body model = %v", gotBody["model"])
	}
	if _, has := gotBody["stream_options"]; has {
		t.Error("stream_options must not be sent")
	}
}

func TestStreamClient_RetryPreDeltaOn429(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okSSE))
	}))
	defer srv.Close()
	c, _ := NewStreamClient("k", srv.URL, srv.Client(), nil, nil, false)
	res, err := c.DoStream(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}}, nil)
	if err != nil {
		t.Fatalf("after retry: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("Text = %q", res.Text)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestStreamClient_ThinkingErrorMaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"\"thinking.type.enabled\" is not supported"}}`))
	}))
	defer srv.Close()
	c, _ := NewStreamClient("k", srv.URL, srv.Client(), nil, nil, false)
	_, err := c.DoStream(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}}, nil)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestNewStreamClient_URLReject(t *testing.T) {
	if _, err := NewStreamClient("k", "://bad", nil, nil, nil, false); err == nil {
		t.Fatal("want error for invalid URL")
	}
}
