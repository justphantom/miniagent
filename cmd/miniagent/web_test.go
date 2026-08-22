package main

// web_test.go covers the -serve HTTP surface: auth gate, whoami probe, static shell serving,
// and the loopback classification that gates remote-without-key startup. The turn pipeline
// itself is covered in web_turn_test.go with an injected fake LLM.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
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
	// L20: a successful delete releases the registry slot — nothing lingers for the process lifetime.
	if _, ok := s.turns.running("del-1"); ok {
		t.Error("registry entry still present after successful delete")
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
	// Hold the registry slot as handleTurn would during a running turn (kind=running blocks
	// both new turns and deletes, mirroring the old held mutex).
	if _, busy := s.turns.register("busy-1", func() {}); busy {
		t.Fatal("register unexpectedly busy on a fresh registry")
	}
	defer s.turns.finish("busy-1", nil)
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

// The guard is the DNS-rebinding / CSRF line: with allowedHosts set, only expected Hosts
// pass; cross-site Sec-Fetch-Site and cross-origin Origin headers are rejected.
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

// Wildcard bind: the browser reaches the server via any local interface address, so those
// (and the machine hostname) must pass the Host gate — this is the LAN-access deployment.
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

// web.allowed_hosts: reverse-proxy deployments add their public name explicitly.
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

// CSRF: a no-cors form POST cannot carry application/json — anything else is 415.
// Combined with the guard above this closes the loopback forged-POST hole.
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

func TestWebSessionsList_UnreadableDir(t *testing.T) {
	s := testWebServer("")
	s.cfg.Session.Dir = filepath.Join(t.TempDir(), "missing")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list unreadable dir code = %d, want 500", rec.Code)
	}
}
