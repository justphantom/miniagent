package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestListenHostIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"localhost:8787", true},
		{"[::1]:8787", true},
		{"", false},
		{":8787", false},        // empty host binds all interfaces
		{"0.0.0.0:8787", false}, // wildcard
		{"192.168.1.5:8787", false},
		{"bad", false}, // unparseable → treated as remote (fail closed)
	}
	for _, c := range cases {
		if got := listenHostIsLoopback(c.addr); got != c.want {
			t.Errorf("listenHostIsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestHostVariants(t *testing.T) {
	// Wildcard binds also enumerate interface IPs, which vary by machine — assert the
	// deterministic prefix and, for wildcards, that the hostname joined.
	cases := []struct {
		addr string
		want []string
	}{
		{"127.0.0.1:8787", []string{"127.0.0.1", "localhost", "::1"}},
		{"localhost:8787", []string{"localhost", "127.0.0.1", "::1"}},
		{"192.168.1.5:8787", []string{"192.168.1.5"}},
		{"bad", []string{"bad"}},
	}
	for _, c := range cases {
		got := hostVariants(c.addr, nil)
		if !slices.Equal(got, c.want) {
			t.Errorf("hostVariants(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		for _, wc := range []string{"0.0.0.0:8787", "[::]:8787"} {
			got := hostVariants(wc, nil)
			if !slices.ContainsFunc(got, func(v string) bool { return strings.EqualFold(v, hn) }) {
				t.Errorf("hostVariants(%q) = %v, want hostname %q included", wc, got, hn)
			}
		}
	}
	// extra (config web.allowed_hosts) appends after the derived variants; trimmed, deduped.
	got := hostVariants("127.0.0.1:8787", []string{"agent.example.com", " 10.0.0.5 ", "localhost"})
	want := []string{"127.0.0.1", "localhost", "::1", "agent.example.com", "10.0.0.5"}
	if !slices.Equal(got, want) {
		t.Errorf("hostVariants extra = %v, want %v", got, want)
	}
}

func TestWebGuard(t *testing.T) {
	s := testWebServer("")
	s.allowedHosts = hostVariants("127.0.0.1:8787", nil)
	h := s.mux()

	cases := []struct {
		name   string
		host   string
		header map[string]string
		want   int
	}{
		{"host with port", "127.0.0.1:8787", nil, http.StatusOK},
		{"host localhost variant", "localhost", nil, http.StatusOK},
		{"rebound host", "attacker.com", nil, http.StatusForbidden},
		{"same-origin fetch-site", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"no fetch-site (curl)", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": ""}, http.StatusOK},
		{"cross-site fetch-site", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-origin origin", "127.0.0.1:8787", map[string]string{"Origin": "http://127.0.0.1:8787"}, http.StatusOK},
		{"cross-origin origin", "127.0.0.1:8787", map[string]string{"Origin": "http://evil.com"}, http.StatusForbidden},
	}
	s.cfg.Session.Dir = t.TempDir()
	for _, c := range cases {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
		req.Host = c.host
		for k, v := range c.header {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: code = %d, want %d", c.name, rec.Code, c.want)
		}
	}

	// Security headers on a passing response.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, hname := range []string{"X-Frame-Options", "Content-Security-Policy", "X-Content-Type-Options"} {
		if rec.Header().Get(hname) == "" {
			t.Errorf("missing %s header", hname)
		}
	}
}

func TestWebGuard_WildcardBind_InterfaceHosts(t *testing.T) {
	s := testWebServer("")
	s.cfg.Session.Dir = t.TempDir()
	s.allowedHosts = hostVariants("0.0.0.0:8787", nil)
	h := s.mux()
	for _, host := range append(localInterfaceHosts(), "127.0.0.1", "localhost") {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %q: code = %d, want 200 (interface host of a wildcard bind)", host, rec.Code)
		}
	}
}

func TestWebGuard_ConfigAllowedHosts(t *testing.T) {
	s := testWebServer("")
	s.cfg.Session.Dir = t.TempDir()
	s.allowedHosts = hostVariants("127.0.0.1:8787", []string{"agent.example.com"})
	h := s.mux()
	for _, host := range []string{"agent.example.com", "127.0.0.1:8787"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %q: code = %d, want 200", host, rec.Code)
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
	req.Host = "other.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unlisted host code = %d, want 403", rec.Code)
	}
}

func TestWebTurnContentTypeEnforced(t *testing.T) {
	s, _ := newTurnTestServer(t)
	for _, ct := range []string{"", "text/plain", "multipart/form-data", "application/x-www-form-urlencoded"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(`{"prompt":"hi","workdir":"/tmp"}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content-type %q: code = %d, want 415", ct, rec.Code)
		}
	}
}

func TestWebTurn_ResumeMissingSession404(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"hi","workdir":%q,"session":"nope-123"}`, workdir))
	if rec.Code != http.StatusNotFound {
		t.Errorf("resume missing code = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
