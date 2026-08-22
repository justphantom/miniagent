package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/miniagent/session"
)

// generateSessionID outputs only latin letters/digits/- (passes ValidateSessionID), with a timestamp prefix.
func TestGenerateSessionID_Format(t *testing.T) {
	id := generateSessionID()
	if err := session.ValidateSessionID(id); err != nil {
		t.Errorf("id %q invalid: %v", id, err)
	}
	ts := time.Now().Format("20060102-150405")
	if !strings.HasPrefix(id, ts+"-") {
		t.Errorf("id %q missing timestamp prefix %q-", id, ts)
	}
}

// Concurrent generation within the same second should not collide (random segment 64-bit guarantees it; fallback path adds pid to distinguish same-second different-process).
func TestGenerateSessionID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := range 1000 {
		id := generateSessionID()
		if seen[id] {
			t.Fatalf("collision on iteration %d: %q", i, id)
		}
		seen[id] = true
	}
}

// Stateless: saveNew=false and sessionArg="" → empty path, zero meta, nil history.
func TestResolveSessionForRun_Stateless(t *testing.T) {
	path, meta, history := resolveSessionForRun(false, "", t.TempDir(), "p/m", "p", "/wd", 0)
	if path != "" {
		t.Errorf("path = %q, want empty (stateless does not persist)", path)
	}
	if meta != (session.SessionMeta{}) {
		t.Errorf("meta = %+v, want zero value", meta)
	}
	if history != nil {
		t.Errorf("history should be nil, got %v", history)
	}
}

// New-session branch: when file does not exist, build meta with Type left empty (filled with session when AppendMessages writes to disk),
// Provider listed separately from modelSpec, to ease session listing / multi-provider tracing without parsing strings.
func TestResolveSessionForRun_NewSession_FillsProvider(t *testing.T) {
	sessionDir := t.TempDir()
	path, meta, history := resolveSessionForRun(true, "", sessionDir, "openai/gpt-4o", "openai", "/repo", 0)
	wantPath := filepath.Join(sessionDir, meta.ID+".jsonl")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if meta.Type != "" {
		t.Errorf("Type = %q, want empty (filled with session when AppendMessages writes to disk)", meta.Type)
	}
	if meta.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", meta.Provider)
	}
	if meta.Model != "openai/gpt-4o" {
		t.Errorf("Model = %q, want openai/gpt-4o", meta.Model)
	}
	if meta.Workdir != "/repo" {
		t.Errorf("Workdir = %q, want /repo (absWorkdir returns absolute paths as-is)", meta.Workdir)
	}
	if meta.ID == "" {
		t.Error("ID is empty")
	}
	if err := session.ValidateSessionID(meta.ID); err != nil {
		t.Errorf("meta.ID %q invalid: %v", meta.ID, err)
	}
	if _, err := time.Parse(time.RFC3339, meta.Created); err != nil {
		t.Errorf("Created %q not RFC3339: %v", meta.Created, err)
	}
	if history != nil {
		t.Errorf("history should be nil (new session has no history), got %v", history)
	}
}

// defaultSessionDir falls back to the workdir-relative default unless $MINIAGENT_SESSION_DIR is set (L11).
func TestDefaultSessionDir_Env(t *testing.T) {
	const env = "MINIAGENT_SESSION_DIR"
	old, had := os.LookupEnv(env)
	t.Cleanup(func() {
		if had {
			os.Setenv(env, old)
		} else {
			os.Unsetenv(env)
		}
	})
	os.Unsetenv(env)
	if d := defaultSessionDir(); d != ".miniagent/sessions" {
		t.Fatalf("default = %q, want .miniagent/sessions", d)
	}
	os.Setenv(env, "/var/lib/miniagent/sessions")
	if d := defaultSessionDir(); d != "/var/lib/miniagent/sessions" {
		t.Fatalf("env default = %q, want the session dir", d)
	}
}
