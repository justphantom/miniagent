package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// defaultSessionDir is the fallback directory when session.dir is not configured (overridden by config in Phase C).
const defaultSessionDir = ".miniagent/sessions"

// resolveSessionForRun adjudicates the session across three states (mutual exclusion is guaranteed by validateConversation so they are never both true):
//   - saveNew=true: create a new session, generateSessionID generates the id, construct meta (the stdout NDJSON output of the id is handled by main's EmitSession), history=nil.
//   - sessionArg!="": resume; validate the id then LoadSession; if the file does not exist (meta.Type=="") error out to prevent creating a garbage session on a typo.
//   - both empty: stateless, return an empty path (main skips persistence accordingly).
func resolveSessionForRun(saveNew bool, sessionArg, sessionDir, modelSpec, provider, workdir string, maxSessionBytes int64) (string, session.SessionMeta, []miniagent.Message) {
	if !saveNew && sessionArg == "" {
		return "", session.SessionMeta{}, nil
	}
	id := sessionArg
	if saveNew {
		id = generateSessionID()
	}
	sessPath, err := session.ResolveSessionPath(id, sessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: session: %v\n", err)
		os.Exit(1)
	}
	meta, history, err := session.LoadSession(sessPath, maxSessionBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: load session: %v\n", err)
		os.Exit(1)
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
			// Resume but the file does not exist → error out (prevent creating a garbage session on a typo; use -save-session to create a new one).
			fmt.Fprintf(os.Stderr, "miniagent: session %q not found (use -save-session to create a new one)\n", id)
			os.Exit(1)
		}
	} else {
		warnSessionMismatch(meta, modelSpec, workdir)
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
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return wd
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
