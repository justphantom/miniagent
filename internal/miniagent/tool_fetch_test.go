package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCheckSSRF(t *testing.T) {
	cases := []struct {
		url string
		msg string
	}{
		{"http://127.0.0.1:8080", "拒绝"},
		{"http://localhost", "拒绝"},
		{"ftp://example.com", "http/https"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatalf("parse %q: %v", c.url, err)
		}
		if err := checkSSRF(context.Background(), u); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("checkSSRF(%q) = %v, want containing %q", c.url, err, c.msg)
		}
	}
}

// SSRF 开启时，loopback httptest 目标被拒。
func TestFetch_LoopbackRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
	}))
	defer srv.Close()
	r := runFetch(context.Background(), `{"url":"`+srv.URL+`"}`, checkSSRF)
	if !r.IsError || !strings.Contains(r.Output, "拒绝") {
		t.Errorf("loopback should be rejected: %s", r.Output)
	}
}

// ssrf 显式传 no-op func 绕过 loopback 检查（P3-12 后 nil 不再旁路），验证
// GET + HTML 清洗 + 实体反转义。
func TestFetch_SuccessAndClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><script>alert(1)</script><p>Hello &amp; world</p></html>")
	}))
	defer srv.Close()
	noopSSRF := func(context.Context, *url.URL) error { return nil }
	r := runFetch(context.Background(), `{"url":"`+srv.URL+`"}`, noopSSRF)
	if r.IsError {
		t.Fatalf("fetch: %s", r.Output)
	}
	if strings.Contains(r.Output, "alert") || strings.Contains(r.Output, "<p>") {
		t.Errorf("script/tag not stripped: %s", r.Output)
	}
	if !strings.Contains(r.Output, "Hello & world") {
		t.Errorf("entities not decoded: %s", r.Output)
	}
}

// P3-12：ssrf=nil 现兜底用 checkSSRF，消除 nil 旁路维护陷阱。loopback httptest
// 目标即使不显式传 checkSSRF 也应被拒。
func TestFetch_NilSSRFDefaultsToCheckSSRF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
	}))
	defer srv.Close()
	r := runFetch(context.Background(), `{"url":"`+srv.URL+`"}`, nil)
	if !r.IsError || !strings.Contains(r.Output, "拒绝") {
		t.Errorf("nil ssrf should default to checkSSRF and reject loopback: %s", r.Output)
	}
}

// P2[fetch 响应头无上限]：恶意识服务器接收请求后永不发响应头， Transport 的
// ResponseHeaderTimeout 应快速切断，而非拖到 fetchTimeout(20s)。短化
// fetchHeaderTimeout 加速测试（生产值 10s）。
func TestFetch_HeaderTimeoutCutsHungResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 永不 WriteHeader；客户端 ResponseHeaderTimeout 触发后断连，服务端检测到
		// 连接关闭会取消 r.Context()，handler 随之返回，srv.Close() 不挂。
		<-r.Context().Done()
	}))
	defer srv.Close()
	orig := fetchHeaderTimeout
	fetchHeaderTimeout = 100 * time.Millisecond
	defer func() { fetchHeaderTimeout = orig }()
	noopSSRF := func(context.Context, *url.URL) error { return nil }
	start := time.Now()
	r := runFetch(context.Background(), `{"url":"`+srv.URL+`"}`, noopSSRF)
	elapsed := time.Since(start)
	if !r.IsError {
		t.Errorf("hung-header server should yield error, got: %s", r.Output)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ResponseHeaderTimeout not enforced: elapsed=%v", elapsed)
	}
}
