package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

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
	if _, busy := s.turns.register("busy-1", func() {}, nil); busy {
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
