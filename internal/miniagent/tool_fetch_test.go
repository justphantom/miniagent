package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

// ssrf=nil 绕过 SSRF（测试专用），验证 GET + HTML 清洗 + 实体反转义。
func TestFetch_SuccessAndClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><script>alert(1)</script><p>Hello &amp; world</p></html>")
	}))
	defer srv.Close()
	r := runFetch(context.Background(), `{"url":"`+srv.URL+`"}`, nil)
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
