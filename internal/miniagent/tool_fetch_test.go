package miniagent

import (
	"context"
	"fmt"
	"net"
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

// P2[DNS rebinding 闭合]：validateDialIP 是 custom DialContext 的 Control 钩子校验函数。
// 端到端 rebinding 需 DNS 控制（难测），此处单测校验函数本身：批准集非空时，拨号 IP 必须
// ∈ 批准集，否则拒绝（rebinding 命中）；空集放行（checkSSRF 旁路，如测试 noop）。
func TestValidateDialIP(t *testing.T) {
	ip := net.ParseIP
	cases := []struct {
		name     string
		approved []net.IP
		address  string
		wantErr  bool
		wantMsg  string // wantErr=true 时 err.Error() 须包含的子串
	}{
		{"match v4", []net.IP{ip("1.1.1.1")}, "1.1.1.1:80", false, ""},
		{"no match v4 (rebinding)", []net.IP{ip("1.1.1.1")}, "2.2.2.2:80", true, "rebinding"},
		{"match v6", []net.IP{ip("2001:db8::1")}, "[2001:db8::1]:443", false, ""},
		{"no match v6", []net.IP{ip("2001:db8::1")}, "[2001:db8::2]:443", true, "rebinding"},
		{"empty set bypass", nil, "2.2.2.2:80", false, ""},
		{"empty set bypass v6", []net.IP{}, "[::1]:80", false, ""},
		// IPv4-mapped IPv6 归一化：net.IP.Equal 内部 To4，::ffff:1.1.1.1 == 1.1.1.1。
		{"ipv4-mapped equality", []net.IP{ip("::ffff:1.1.1.1")}, "1.1.1.1:80", false, ""},
		{"multi-ip set, one matches", []net.IP{ip("1.1.1.1"), ip("8.8.8.8")}, "8.8.8.8:80", false, ""},
		{"bad address (no port)", []net.IP{ip("1.1.1.1")}, "no-port", true, "拨号地址非法"},
		{"non-IP host", []net.IP{ip("1.1.1.1")}, "example.com:80", true, "不是 IP"},
	}
	for _, c := range cases {
		err := validateDialIP(c.approved, c.address)
		switch {
		case c.wantErr && err == nil:
			t.Errorf("%s: expected error, got nil", c.name)
		case c.wantErr && !strings.Contains(err.Error(), c.wantMsg):
			t.Errorf("%s: err=%q, want containing %q", c.name, err.Error(), c.wantMsg)
		case !c.wantErr && err != nil:
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}
	}
}

// P2[DNS rebinding 闭合]：checkSSRF 解析通过后把批准 IP 记入 ctx 的 approvedSet，
// 供 Control 钩子 pin。用字面公网 IP（8.8.8.8）避免 DNS 网络依赖——LookupIPAddr 对
// 字面 IP 不走网络，直接返回。
func TestCheckSSRF_RecordsApprovedIPs(t *testing.T) {
	approved := &approvedSet{ips: make(map[string][]net.IP)}
	ctx := context.WithValue(context.Background(), approvedIPsKey{}, approved)
	u, err := url.Parse("http://8.8.8.8/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := checkSSRF(ctx, u); err != nil {
		t.Fatalf("checkSSRF public literal IP: %v", err)
	}
	approved.mu.Lock()
	got := approved.ips["8.8.8.8"]
	approved.mu.Unlock()
	if len(got) != 1 || !got[0].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("approvedSet not populated for 8.8.8.8: %+v", got)
	}

	// 私网/loopback 目标在记录前被拒，不会污染 approvedSet。
	loopback, _ := url.Parse("http://127.0.0.1/")
	if err := checkSSRF(ctx, loopback); err == nil {
		t.Fatal("loopback should be rejected")
	}
	approved.mu.Lock()
	_, present := approved.ips["127.0.0.1"]
	approved.mu.Unlock()
	if present {
		t.Error("rejected loopback IP should not be recorded in approvedSet")
	}

	// approvedSet 缺失时 checkSSRF 仍正常（不 panic、不记录）——TestCheckSSRF 已覆盖。
}
