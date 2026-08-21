package main

// web_test.go covers the -serve HTTP surface: auth gate, whoami probe, static shell serving,
// and the loopback classification that gates remote-without-key startup. The turn pipeline
// itself is covered in web_turn_test.go with an injected fake LLM.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
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

func TestWebSessionDelete(t *testing.T) {
	dir := t.TempDir()
	s := testWebServer("")
	s.cfg.Session.Dir = dir

	// Seed a session file via the session package (same write path as the CLI).
	path, err := session.ResolveSessionPath("del-1", dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	meta := session.SessionMeta{Type: "session", ID: "del-1", Created: "2026-01-01T00:00:00Z"}
	if err := session.AppendMessages(path, meta, []miniagent.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	del := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/sessions/"+id, nil)
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, req)
		return rec
	}

	if rec := del("del-1"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d, want 204", rec.Code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after delete: %v", err)
	}
	if rec := del("del-1"); rec.Code != http.StatusNotFound {
		t.Errorf("delete missing code = %d, want 404", rec.Code)
	}
	// Illegal id character ("." is outside the allowlist) → 400. Using an in-segment illegal
	// char avoids ServeMux path-cleaning redirects that "../" would trigger.
	if rec := del("..evil"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id code = %d, want 400", rec.Code)
	}
}

func TestWebSessionDelete_InFlight_Conflict(t *testing.T) {
	dir := t.TempDir()
	s := testWebServer("")
	s.cfg.Session.Dir = dir
	path, err := session.ResolveSessionPath("busy-1", dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	meta := session.SessionMeta{Type: "session", ID: "busy-1", Created: "2026-01-01T00:00:00Z"}
	if err := session.AppendMessages(path, meta, []miniagent.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Hold the turn lock as handleTurn would during a running turn.
	mu := &sync.Mutex{}
	s.locks.Store("busy-1", mu)
	mu.Lock()
	defer mu.Unlock()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/sessions/busy-1", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("in-flight delete code = %d, want 409", rec.Code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file removed despite in-flight turn: %v", err)
	}
}

// Last assistant message preview: sessionPreview reads the file tail (≤64KB) backwards,
// so a long session costs O(tail), not O(file).
func TestSessionPreview(t *testing.T) {
	dir := t.TempDir()
	path, err := session.ResolveSessionPath("prev-1", dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	meta := session.SessionMeta{Type: "session", ID: "prev-1", Created: "2026-01-01T00:00:00Z"}
	if err := session.AppendMessages(path, meta, []miniagent.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "final\nanswer"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := sessionPreview(path); got != "final answer" {
		t.Errorf("preview = %q, want %q", got, "final answer")
	}
	// No assistant message → empty preview; missing file → empty (no error surfaced to listing).
	if got := sessionPreview(filepath.Join(dir, "nope.jsonl")); got != "" {
		t.Errorf("missing file preview = %q, want empty", got)
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
