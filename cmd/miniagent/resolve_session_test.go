package main

import (
	"os"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// resolveSession (os.Exit-free core) unlocks the previously-untestable error paths: resume-missing and load-failure used
// to os.Exit(1) inside resolveSessionForRun, killing the test process. Now they return errors, exercised hermetically via a
// temp dir (real FS — faithful to max_session_bytes / atomic-rename / crash-recovery semantics; no memStore).

// Resume a non-existent session id → "not found" error (does not create a garbage session on a typo).
func TestResolveSession_ResumeMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, _, err := resolveSession(false, "20260101-000000-deadbeef", dir, "p/m", "p", "/wd", 0)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("resume missing: err = %v, want 'not found'", err)
	}
}

// Resume a session whose file is corrupt → "load session" error (propagates LoadSession's parse failure).
func TestResolveSession_LoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	id := "20260101-000000-cafebabe"
	p, err := session.ResolveSessionPath(id, dir)
	if err != nil {
		t.Fatalf("ResolveSessionPath: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"session","id":"`+id+`","model":"p/m"}
{oops}
{"type":"message","role":"user","content":"q"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = resolveSession(false, id, dir, "p/m", "p", "/wd", 0)
	if err == nil || !strings.Contains(err.Error(), "load session") {
		t.Errorf("load corrupt: err = %v, want 'load session'", err)
	}
}

// No session requested (both flags empty) → empty path, nil error, nil history (stateless run).
func TestResolveSession_Stateless(t *testing.T) {
	sessPath, _, history, err := resolveSession(false, "", t.TempDir(), "p/m", "p", "/wd", 0)
	if err != nil {
		t.Errorf("stateless: err = %v, want nil", err)
	}
	if sessPath != "" || history != nil {
		t.Errorf("stateless: sessPath=%q history=%v, want empty/nil", sessPath, history)
	}
}
