package main

// web_test.go covers the -serve HTTP surface: auth gate, whoami probe, static shell serving,
// and the loopback classification that gates remote-without-key startup. The turn pipeline
// itself is covered in web_turn_test.go with an injected fake LLM.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testWebServer(key string) *webServer {
	cfg := &config.Config{Providers: []config.ProviderConfig{{Name: "p", ChatURL: "http://127.0.0.1:1/v1/chat/completions", Models: []config.ModelConfig{{Name: "m"}}}}, Defaults: config.DefaultsConfig{Provider: "p", Model: "m"}}
	return &webServer{cfg: cfg, key: key, engine: &turnEngine{cfg: cfg, logger: testLogger()}}
}

func TestWebRequireAuth(t *testing.T) {
	s := testWebServer("secret")
	h := s.mux()
	cases := []struct {
		name string
		key  string
		code int
	}{
		{"missing key", "", http.StatusUnauthorized},
		{"wrong key", "nope", http.StatusUnauthorized},
		{"correct key", "secret", http.StatusOK}, // GET /api/sessions on empty dir
	}
	for _, c := range cases {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
		if c.key != "" {
			req.Header.Set("X-Api-Key", c.key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s: code = %d, want %d", c.name, rec.Code, c.code)
		}
	}
}

func TestWebRequireAuth_EmptyKey_NoGate(t *testing.T) {
	s := testWebServer("") // loopback mode: key empty → auth off
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 (no auth in loopback mode)", rec.Code)
	}
}

func TestWebWhoami(t *testing.T) {
	for _, key := range []string{"", "k"} {
		s := testWebServer(key)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/whoami", nil)
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("whoami code = %d", rec.Code)
		}
		var got struct {
			AuthRequired bool   `json:"auth_required"`
			Version      string `json:"version"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.AuthRequired != (key != "") {
			t.Errorf("auth_required = %v, want %v", got.AuthRequired, key != "")
		}
	}
}

func TestWebStaticShell(t *testing.T) {
	s := testWebServer("")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "miniagent") {
		t.Error("shell body missing app markup")
	}
}

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
