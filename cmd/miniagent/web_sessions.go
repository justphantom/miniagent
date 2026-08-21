package main

// web_sessions.go implements GET /api/sessions (list) and GET /api/sessions/{id} (replay the
// last 200 messages as an NDJSON event stream isomorphic with the runtime, so the frontend
// reuses one parser for live turns and historical replay).

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent/session"
)

// maxHistoryMessages caps the replayed tail: keeps the loaded transcript finite for the UI
// without truncating the on-disk file (the full history is still persisted).
const maxHistoryMessages = 200

// sessionSummary is one row of GET /api/sessions. Created/provider/model come from the jsonl
// first meta line; size and mtime from the file itself.
type sessionSummary struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Created  string `json:"created"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

// maxSessionBytesOfConfig reads run.max_session_bytes straight from the config (no Resolve):
// the sessions listing must not require the defaults pair to resolve, only the byte cap matters.
func maxSessionBytesOfConfig(cfg *config.Config) int64 {
	if cfg.Run.MaxSessionBytes != nil {
		return int64(*cfg.Run.MaxSessionBytes)
	}
	return 0
}

func (s *webServer) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	dir := defaultSessionDir
	if s.cfg.Session.Dir != "" {
		dir = s.cfg.Session.Dir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, []sessionSummary{})
		return
	}
	out := make([]sessionSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		summary := sessionSummary{ID: id}
		if fi, err := e.Info(); err == nil {
			summary.Size = fi.Size()
			summary.Modified = fi.ModTime().Format("2006-01-02 15:04")
		}
		// First meta line only (LoadSessionMeta): listing must not pay a full-file read per entry.
		if meta, err := session.LoadSessionMeta(filepath.Join(dir, e.Name())); err == nil && meta.Type != "" {
			summary.Provider, summary.Model, summary.Created = meta.Provider, meta.Model, meta.Created
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Modified > out[j].Modified
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *webServer) handleSessionReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	dir := defaultSessionDir
	if s.cfg.Session.Dir != "" {
		dir = s.cfg.Session.Dir
	}
	sessPath, err := session.ResolveSessionPath(id, dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	meta, msgs, err := session.LoadSession(sessPath, maxSessionBytesOfConfig(s.cfg))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if meta.Type == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	// Tail cap: replay only the most recent N messages so the UI stays bounded; the persisted file
	// keeps the full history for resume. The session event still carries meta unchanged.
	tail := msgs
	if len(tail) > maxHistoryMessages {
		tail = tail[len(tail)-maxHistoryMessages:]
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = replaySession(w, meta, tail)
}
