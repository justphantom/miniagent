package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestClient_AuthHeadersAndResponse(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer srv.Close()
	c, err := NewClient("k-1", srv.URL, srv.Client(), nil, map[string]string{"X-Tenant-Id": "tn"}, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" || res.FinishReason != "stop" || res.Usage.InputTokens != 3 || res.Usage.OutputTokens != 2 {
		t.Errorf("Response = %+v", res)
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
	if gotHeaders.Get("X-Tenant-Id") != "tn" {
		t.Errorf("custom header X-Tenant-Id = %q", gotHeaders.Get("X-Tenant-Id"))
	}
}

func TestClient_RetryOn429(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	c, _ := NewClient("k", srv.URL, srv.Client(), nil, nil, false)
	res, err := c.Do(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}})
	if err != nil {
		t.Fatalf("after retry: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (initial + 2 retries)", attempts)
	}
}

func TestClient_ThinkingErrorMaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"\"thinking.type.enabled\" is not supported"}}`))
	}))
	defer srv.Close()
	c, _ := NewClient("k", srv.URL, srv.Client(), nil, nil, false)
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}})
	if !errors.Is(err, miniagent.ErrThinkingUnsupported) {
		t.Errorf("err = %v, want ErrThinkingUnsupported", err)
	}
}

func TestClient_ContextLengthMaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 200000 tokens. However, your messages resulted in 1000000 tokens."}}`))
	}))
	defer srv.Close()
	c, _ := NewClient("k", srv.URL, srv.Client(), nil, nil, false)
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}})
	if !errors.Is(err, miniagent.ErrContextLength) {
		t.Errorf("err = %v, want ErrContextLength", err)
	}
}

func TestNewClient_URLReject(t *testing.T) {
	if _, err := NewClient("k", "://bad", nil, nil, nil, false); err == nil {
		t.Fatal("want error for invalid URL")
	}
}
