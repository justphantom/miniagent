package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/httpretry"
)

// Retry tests: use httptest.Server + atomic counters to precisely control each response.
// The retry baseline retryBaseDelay=500ms would cause cumulative delays in retry tests, so all cases use
// Retry-After: 0 or have the server return immediately, keeping backoff at the 500ms scale.

// retryServer returns status/body/headers from plan for the Nth (0-based) request.
// After plan is exhausted it returns 200 OK with an empty body.
func retryServer(t *testing.T, plan []struct {
	status  int
	body    string
	headers map[string]string
}) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&calls, 1) - 1
		if int(idx) < len(plan) {
			p := plan[idx]
			for k, v := range p.headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(p.status)
			_, _ = io.WriteString(w, p.body)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, textResponseJSON("late-ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func textResponseJSON(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// 429 once then 200: retry once and get the result; call count = 2.
func TestChatClient_Do_RetriesOn429ThenSucceeds(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusTooManyRequests, body: `{"error":"rate"}`, headers: map[string]string{"Retry-After": "0"}},
	})
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "late-ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Errorf("calls = %d, want 2", atomic.LoadInt32(calls))
	}
}

// 503 three times: still failing after maxRetries retries, error propagates; call count = 1 + maxRetries.
func TestChatClient_Do_RetriesExhaustedOn503(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusServiceUnavailable, body: "busy"},
		{status: http.StatusServiceUnavailable, body: "busy"},
		{status: http.StatusServiceUnavailable, body: "busy"},
	})
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v", err)
	}
	// After retries are exhausted the error message should contain "after N retries" context for debugging.
	if !strings.Contains(err.Error(), "after 2 retries") {
		t.Errorf("err should mention retry count: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != int32(1+httpretry.MaxRetries) {
		t.Errorf("calls = %d, want %d", got, 1+httpretry.MaxRetries)
	}
}

// 500 (Internal Server Error) should also be retried — consistent with mainstream LLM SDK behavior.
// Here we verify: one 500 then 200 on the second attempt can successfully retry and get the result.
func TestChatClient_Do_RetriesOn500(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusInternalServerError, body: "boom"},
	})
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err != nil {
		t.Fatalf("expected retry success, got: %v", err)
	}
	if resp.Text != "late-ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("calls = %d, want 2 (500 retried, then succeeded)", got)
	}
}

// 400 returns immediately with no retry.
func TestChatClient_Do_NoRetryOn400(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusBadRequest, body: `{"error":"bad"}`},
	})
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 400)", got)
	}
}

// Retry-After seconds are honored: server asks to wait 1s, Do should wait about 1s before retrying.
func TestChatClient_Do_RespectsRetryAfterSeconds(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusTooManyRequests, body: "", headers: map[string]string{"Retry-After": "1"}},
	})
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	start := time.Now()
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("Retry-After=1s not honored: elapsed=%v", elapsed)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// ctx cancel interrupts the retry loop, not continuing to burn requests.
func TestChatClient_Do_RetryCancelledByCtx(t *testing.T) {
	// Use a server that always returns 503 + Retry-After: 60; with ctx cancelled at 1ms it must land in the backoff window.
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusServiceUnavailable, body: "", headers: map[string]string{"Retry-After": "60"}},
		{status: http.StatusServiceUnavailable, body: "", headers: map[string]string{"Retry-After": "60"}},
		{status: http.StatusServiceUnavailable, body: "", headers: map[string]string{"Retry-After": "60"}},
	})
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Do(ctx, miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	// At least one request was issued; the retry loop was interrupted by ctx.
	if got := atomic.LoadInt32(calls); got < 1 {
		t.Errorf("calls = %d, want >= 1", got)
	}
}

// Network errors (server closing the connection) also trigger a retry.
func TestChatClient_Do_RetriesOnNetworkError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// After hijacking, close the connection immediately; client.Do returns an io.ErrUnexpectedEOF-like error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("server does not support hijack")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
		calls.Add(1)
	}))
	t.Cleanup(srv.Close)
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	// Retry is triggered at least twice (initial attempt + at least 1 retry).
	if got := calls.Load(); got < 2 {
		t.Errorf("calls = %d, want >= 2 (network retry)", got)
	}
}

// P2-4: DoStream pre-delta phase returns 503 once then a 200 SSE stream, successfully aggregated (pre-delta failures can be retried).
func TestStreamClient_DoStream_RetriesOn503ThenSucceeds(t *testing.T) {
	const okSSE = `data: {"choices":[{"delta":{"content":"recovered"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusServiceUnavailable, body: "busy", headers: map[string]string{"Retry-After": "0"}},
		{status: http.StatusOK, body: okSSE, headers: map[string]string{"Content-Type": "text/event-stream"}},
	})
	c := &StreamClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "recovered" {
		t.Errorf("Text = %q", resp.Text)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("calls = %d, want 2 (503 retried then success)", got)
	}
}

// P2-4: DoStream pre-delta 503 keeps failing; after retries are exhausted the error contains "503" and "after N retries".
func TestStreamClient_DoStream_RetriesExhaustedOn503(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusServiceUnavailable, body: "busy", headers: map[string]string{"Retry-After": "0"}},
		{status: http.StatusServiceUnavailable, body: "busy", headers: map[string]string{"Retry-After": "0"}},
		{status: http.StatusServiceUnavailable, body: "busy", headers: map[string]string{"Retry-After": "0"}},
	})
	c := &StreamClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "after 2 retries") {
		t.Errorf("err = %v", err)
	}
	if got := atomic.LoadInt32(calls); got != int32(1+httpretry.MaxRetries) {
		t.Errorf("calls = %d, want %d", got, 1+httpretry.MaxRetries)
	}
}
