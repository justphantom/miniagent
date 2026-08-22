package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// defaultSessionDir is the fallback directory when session.dir is not configured (overridden by config in Phase C).
const defaultSessionDir = ".miniagent/sessions"

// errSessionNotFound marks a resume whose session file does not exist. Sentinel (not a
// fmt.Errorf) so the web layer can map it to 404; the CLI path prints it like any error.
var errSessionNotFound = errors.New("session not found")

// resolveSession is the os.Exit-free, testable core of resolveSessionForRun: it adjudicates the session across three states
// (mutual exclusion is guaranteed by validateConversation so saveNew+sessionArg are never both true) and returns errors instead
// of terminating the process. resolveSessionForRun wraps it with the stderr+os.Exit(1) CLI behavior. Tests exercise the
// previously-untestable error paths (resume missing, load failure) hermetically via a temp dir — real FS (faithful: keeps the
// max_session_bytes / atomic-rename / crash-recovery semantics a memStore mock would silently drop), no new Store interface.
//   - saveNew=true: create a new session (generateSessionID), construct meta, history=nil.
//   - sessionArg!="": resume; validate id + LoadSession; if the file does not exist (meta.Type=="") return a "not found" error.
//   - both empty: stateless, return an empty path (main skips persistence).
func resolveSession(saveNew bool, sessionArg, sessionDir, modelSpec, provider, workdir string, maxSessionBytes int64) (sessPath string, meta session.SessionMeta, history []miniagent.Message, err error) {
	if !saveNew && sessionArg == "" {
		return "", session.SessionMeta{}, nil, nil
	}
	id := sessionArg
	if saveNew {
		id = generateSessionID()
	}
	sessPath, err = session.ResolveSessionPath(id, sessionDir)
	if err != nil {
		return "", session.SessionMeta{}, nil, fmt.Errorf("session: %w", err)
	}
	meta, history, err = session.LoadSession(sessPath, maxSessionBytes)
	if err != nil {
		return "", session.SessionMeta{}, nil, fmt.Errorf("load session: %w", err)
	}
	if meta.Type == "" {
		if saveNew {
			// New session: construct metadata (Type is filled in as session by AppendMessages). Provider is listed separately from
			// modelSpec ("provider/id"), so session listing / multi-provider tracing can avoid parsing the string.
			meta = session.SessionMeta{
				ID:       id,
				Model:    modelSpec,
				Provider: provider,
				Workdir:  absWorkdir(workdir),
				Created:  time.Now().Format(time.RFC3339),
			}
		} else {
			// Resume but the file does not exist → sentinel error (prevent creating a garbage session
			// on a typo; use -save-session to create a new one). Wraps errSessionNotFound so the web
			// layer can map it to 404 while the message keeps the CLI hint.
			return "", session.SessionMeta{}, nil, fmt.Errorf("%w: session %q (use -save-session to create a new one)", errSessionNotFound, id)
		}
	} else {
		warnSessionMismatch(meta, modelSpec, workdir)
	}
	return sessPath, meta, history, nil
}

// resolveSessionForRun is the CLI entry: resolveSession + stderr + os.Exit(1) on error. The error messages are preserved
// verbatim from the pre-refactor inline form (wrapped under "miniagent: " by this wrapper).
func resolveSessionForRun(saveNew bool, sessionArg, sessionDir, modelSpec, provider, workdir string, maxSessionBytes int64) (string, session.SessionMeta, []miniagent.Message) {
	sessPath, meta, history, err := resolveSession(saveNew, sessionArg, sessionDir, modelSpec, provider, workdir, maxSessionBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
	return sessPath, meta, history
}

// generateSessionID generates a new session id: timestamp + random hex, containing only Latin letters/digits/- (satisfies ValidateSessionID).
// The random segment is 8 bytes (64 bits): raises the collision threshold for concurrent same-second creation to the 2^32 range, covering CI matrix / batch fork scenarios.
// crypto/rand failure is extremely rare; falls back to timestamp+pid (still valid; pid distinguishes different processes within the same second, avoiding the guaranteed collision of a bare timestamp).
func generateSessionID() string {
	ts := time.Now().Format("20060102-150405")
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ts + "-" + strconv.Itoa(os.Getpid())
	}
	return ts + "-" + hex.EncodeToString(b[:])
}

func absWorkdir(workdir string) string {
	// No os.Getwd() fallback: workdir must come from -workdir only (validated
	// non-empty + absolute upstream by validateConversation). Empty stays empty
	// rather than silently leaking the process cwd into the session metadata.
	if workdir == "" {
		return ""
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return workdir
	}
	return abs
}

func warnSessionMismatch(meta session.SessionMeta, modelSpec, workdir string) {
	if modelSpec != "" && meta.Model != "" && meta.Model != modelSpec {
		fmt.Fprintf(os.Stderr, "miniagent: warning: session model %q does not match this run %q\n", meta.Model, modelSpec)
	}
	if aw := absWorkdir(workdir); aw != "" && meta.Workdir != "" && meta.Workdir != aw {
		fmt.Fprintf(os.Stderr, "miniagent: warning: session workdir %q does not match this run %q\n", meta.Workdir, aw)
	}
}
