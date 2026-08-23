package tools

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func remapPublicDial(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if host == "8.8.8.8" {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func TestWeb_PublicHostBlockedCases(t *testing.T) {
	// Guard-only tests (no HTTP): the tool with allowPrivate=false must deny
	// loopback / private / link-local targets before any request goes out.
	tl := WebTool(0, 0, 0, false)
	cases := []struct {
		name, host string
	}{
		{"loopback literal", "http://127.0.0.1/x"},
		{"loopback name", "http://localhost/x"},
		{"private v4 literal", "http://10.1.2.3/x"},
		{"private v4 literal 192.168", "http://192.168.0.1/x"},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/"},
		{"ipv6 loopback", "http://[::1]/x"},
		{"ipv6 unique-local", "http://[fd00::1]/x"},
	}
	for _, c := range cases {
		res := callWeb(t, tl, `{"url":"`+c.host+`"}`)
		if !res.IsError {
			t.Errorf("%s (%s): want block", c.name, c.host)
		} else if !strings.Contains(res.Output, "blocked") && !strings.Contains(res.Output, "resolve") {
			t.Errorf("%s: error %q lacks block reason", c.name, res.Output)
		}
	}
}

func TestCheckWebHost_RejectsLoopbackAndV4Mapped(t *testing.T) {
	// Public-looking first hop (allowPrivate=true only for the first hop via test
	// server), then redirect to a loopback literal: CheckRedirect re-checks and
	// must fail despite allowPrivate... allowPrivate skips redirect checks too,
	// so here we assert the guarded variant rejects via checkWebHost directly.
	if err := checkWebHost(context.Background(), mustParseURL(t, "http://127.0.0.1:9/x"), false); err == nil {
		t.Error("checkWebHost must reject loopback literal")
	}
	if err := checkWebHost(context.Background(), mustParseURL(t, "http://[::ffff:127.0.0.1]/x"), false); err == nil {
		t.Error("checkWebHost must reject v4-mapped loopback")
	}
}

func TestWeb_RedirectToPrivateLiteralBlocked(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not reach"))
	}))
	defer final.Close()
	finalURL, _ := url.Parse(final.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:"+finalURL.Port()+"/secret", http.StatusFound)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	remapPublicDial(t)
	tl := WebTool(0, 0, 0, false) // allowPrivate=false: CheckRedirect re-checks each hop
	res := callWeb(t, tl, `{"url":"http://8.8.8.8:`+srvURL.Port()+`/redirect"}`)
	if !res.IsError {
		t.Fatalf("want redirect-to-private blocked, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "blocked") {
		t.Errorf("error %q lacks redirect-block reason", res.Output)
	}
	if strings.Contains(res.Output, "should not reach") {
		t.Error("redirect to private target was followed")
	}
}

func TestWeb_RedirectToPublicLiteralFollowed(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	}))
	defer final.Close()
	finalURL, _ := url.Parse(final.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://8.8.8.8:"+finalURL.Port()+"/x", http.StatusFound)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	remapPublicDial(t)
	tl := WebTool(0, 0, 0, false)
	res := callWeb(t, tl, `{"url":"http://8.8.8.8:`+srvURL.Port()+`/redirect"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "arrived") {
		t.Errorf("redirect to public literal not followed: %q", res.Output)
	}
}
