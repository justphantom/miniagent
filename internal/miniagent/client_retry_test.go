package miniagent

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
)

// 重试相关：用 httptest.Server + atomic 计数精确控制每次响应。
// 重试基线 retryBaseDelay=500ms 会让重试测试累积耗时，所有用例都用
// Retry-After: 0 或 server 立即返回，使退避降到 500ms 量级。

// retryServer 第 N 次（0-based）请求按 plan 返回 status/body/headers。
// plan 用尽后返回 200 OK 空体。
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

// 429 一次后 200：重试一次拿到结果，调用数=2。
func TestHTTPClient_Do_RetriesOn429ThenSucceeds(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusTooManyRequests, body: `{"error":"rate"}`, headers: map[string]string{"Retry-After": "0"}},
	})
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
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

// 503 三次：重试 maxRetries 次后仍失败，错误上抛，调用数=1+maxRetries。
func TestHTTPClient_Do_RetriesExhaustedOn503(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusServiceUnavailable, body: "busy"},
		{status: http.StatusServiceUnavailable, body: "busy"},
		{status: http.StatusServiceUnavailable, body: "busy"},
	})
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v", err)
	}
	// 重试用尽后错误信息应含"已重试 N 次"上下文，便于排错。
	if !strings.Contains(err.Error(), "after 2 retries") {
		t.Errorf("err should mention retry count: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != int32(1+maxRetries) {
		t.Errorf("calls = %d, want %d", got, 1+maxRetries)
	}
}

// 500（Internal Server Error）也应重试——主流 LLM SDK 一致行为。
// 这里验证：500 一次后第二次 200，能成功重试拿到结果。
func TestHTTPClient_Do_RetriesOn500(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusInternalServerError, body: "boom"},
	})
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
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

// 400 立即返回不重试。
func TestHTTPClient_Do_NoRetryOn400(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusBadRequest, body: `{"error":"bad"}`},
	})
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 400)", got)
	}
}

// Retry-After 秒数被尊重：服务器要求等 1s，Do 应等约 1s 后再重试。
func TestHTTPClient_Do_RespectsRetryAfterSeconds(t *testing.T) {
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusTooManyRequests, body: "", headers: map[string]string{"Retry-After": "1"}},
	})
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	start := time.Now()
	_, err := c.Do(context.Background(), Request{Model: "m"})
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

// ctx 取消中断重试循环，不继续烧请求。
func TestHTTPClient_Do_RetryCancelledByCtx(t *testing.T) {
	// 用一个永远 503 + Retry-After: 60 的服务器，ctx 1ms 取消必然落到退避段。
	srv, calls := retryServer(t, []struct {
		status  int
		body    string
		headers map[string]string
	}{
		{status: http.StatusServiceUnavailable, body: "", headers: map[string]string{"Retry-After": "60"}},
		{status: http.StatusServiceUnavailable, body: "", headers: map[string]string{"Retry-After": "60"}},
		{status: http.StatusServiceUnavailable, body: "", headers: map[string]string{"Retry-After": "60"}},
	})
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Do(ctx, Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	// 至少发起了一次请求；重试循环被 ctx 打断。
	if got := atomic.LoadInt32(calls); got < 1 {
		t.Errorf("calls = %d, want >= 1", got)
	}
}

// 网络错误（服务器关闭连接）也触发重试。
func TestHTTPClient_Do_RetriesOnNetworkError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// hijack 后立即关闭连接，client.Do 返回 io.ErrUnexpectedEOF 类错误。
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("server does not support hijack")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
		calls.Add(1)
	}))
	t.Cleanup(srv.Close)
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	// 重试至少触发 2 次（初试 + 至少 1 次重试）。
	if got := calls.Load(); got < 2 {
		t.Errorf("calls = %d, want >= 2 (network retry)", got)
	}
}

// parseRetryAfter 必须区分"未提供"（-1，走 backoff）与"显式 0"（0，立即重试），
// 否则合法的 Retry-After: 0 会被误当作未提供而退回退避，违背服务端意图。
func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", -1},
		{"explicit-zero", "0", 0},
		{"seconds", "2", 2 * time.Second},
		{"garbage", "not-a-date", -1},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := parseRetryAfter(h); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
