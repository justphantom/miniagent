package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justphantom/miniagent/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testWebServer(key string) *webServer {
	cfg := &config.Config{Providers: []config.ProviderConfig{{Name: "p", ChatURL: "http://127.0.0.1:1/v1/chat/completions", Models: []config.ModelConfig{{Name: "m"}}}}, Defaults: config.DefaultsConfig{Provider: "p", Model: "m"}}
	return newWebServer(context.Background(), cfg, &turnEngine{cfg: cfg, logger: testLogger()}, key, testLogger())
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
		{"correct key", "secret", http.StatusOK}, // GET /api/models (no session-dir dependency)
	}
	for _, c := range cases {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/models", nil)
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
	s.cfg.Session.Dir = t.TempDir()
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
