package main

// web_sessions.go implements GET /api/sessions (list) and GET /api/sessions/{id} (replay the
// last 200 messages as an NDJSON event stream isomorphic with the runtime, so the frontend
// reuses one parser for live turns and historical replay).

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// maxHistoryMessages caps the replayed tail: keeps the loaded transcript finite for the UI
// without truncating the on-disk file (the full history is still persisted).
const maxHistoryMessages = 200

// sessionSummary is one row of GET /api/sessions. Created/provider/model come from the jsonl
// first meta line; workdir is surfaced so the UI can switch the active workdir when a session is
// opened; size and mtime from the file itself; preview is the tail of the last assistant message;
// running reports an in-flight turn (turnRegistry) for the sidebar's running indicator.
type sessionSummary struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Workdir  string `json:"workdir"`
	Created  string `json:"created"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Preview  string `json:"preview"`
	Running  bool   `json:"running"`
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
	if remote := remoteClientOf(s.cfg); remote != nil {
		s.listSessionsRemote(w, r, remote)
		return
	}
	// Per-entry cost: LoadSessionMeta (first line) + sessionPreview (file tail ≤8KB),
	// never a full-file LoadSession — the listing is refreshed after every turn.
	dir := defaultSessionDir()
	if s.cfg.Session.Dir != "" {
		dir = s.cfg.Session.Dir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// 500, not 200+[]: an unreadable session dir is a config error (wrong session.dir),
		// and an empty list would silently mask it.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read sessions dir: " + err.Error()})
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
			summary.Provider, summary.Model, summary.Workdir, summary.Created = meta.Provider, meta.Model, meta.Workdir, meta.Created
		}
		summary.Preview = sessionPreview(filepath.Join(dir, e.Name()))
		if _, running := s.turns.running(id); running {
			summary.Running = true
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Modified > out[j].Modified
	})
	writeJSON(w, http.StatusOK, out)
}

// sessionPreview reads the last 8KB of the session file and returns the tail of the last
// assistant message content (one line, capped). Best-effort: any failure → "".
func sessionPreview(path string) string {
	const tail = 8 << 10
	f, err := miniagent.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := int64(0)
	if st.Size() > tail {
		off = st.Size() - tail
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return ""
	}
	preview := ""
	for line := range strings.SplitSeq(string(buf), "\n") {
		if !strings.Contains(line, `"role":"assistant"`) {
			continue
		}
		var m struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &m); err == nil && m.Content != "" {
			preview = m.Content
		}
	}
	preview = strings.Join(strings.Fields(preview), " ")
	if len([]rune(preview)) > 60 {
		preview = string([]rune(preview)[:60]) + "…"
	}
	return preview
}

func (s *webServer) handleSessionReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	var meta session.SessionMeta
	var msgs []miniagent.Message
	if remote := remoteClientOf(s.cfg); remote != nil {
		// Same id allowlist as the local ResolveSessionPath branch → 400, not a 500 from the client.
		if err := session.ValidateSessionID(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var err error
		meta, msgs, err = remote.LoadSession(r.Context(), id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	} else {
		dir := defaultSessionDir()
		if s.cfg.Session.Dir != "" {
			dir = s.cfg.Session.Dir
		}
		sessPath, err := session.ResolveSessionPath(id, dir)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		meta, msgs, err = session.LoadSession(sessPath, maxSessionBytesOfConfig(s.cfg))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	if meta.Type == "" {
		// Missing file (local zero meta) or a migrated remote file whose first line is not a
		// valid meta — both answer 404 rather than replaying an empty stream.
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
	w.Header().Set("X-Accel-Buffering", "no")
	_ = replaySession(w, meta, tail)
}

// handleSessionDelete removes the session jsonl file. A session with a turn in flight is
// rejected (409): deleting under a running writer would corrupt the file or resurrect it via
// the writer's next append. The registry's beginDelete slot blocks new turns for the remove's
// duration (same id-allowlist validation as replay).
func (s *webServer) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := session.ValidateSessionID(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if remote := remoteClientOf(s.cfg); remote != nil {
		s.deleteSessionRemote(w, r, id, remote)
		return
	}
	dir := defaultSessionDir()
	if s.cfg.Session.Dir != "" {
		dir = s.cfg.Session.Dir
	}
	path, err := session.ResolveSessionPath(id, dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Reserve the session against new turns while removing the file. The old per-session
	// mutex map is gone — the registry entry is released by defer, nothing lingers (L20).
	if s.turns.beginDelete(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session has a turn in progress"})
		return
	}
	defer s.turns.finish(id, nil)

	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Clean up the per-session tool-output directory (created as <id>.tool-output in run_turn.go)
	// to prevent orphan accumulation (M7).
	_ = os.RemoveAll(path + ".tool-output")
	s.turns.broadcastLife(lifeEvent("session_deleted", id, ""))
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionStop cancels the session's in-flight turn (D1's explicit stop): the engine
// takes the normal cancel path — the executed part is saved, subscribers see the stream end.
func (s *webServer) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := session.ValidateSessionID(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	entry, ok := s.turns.running(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no turn in progress"})
		return
	}
	entry.cancel()
	w.WriteHeader(http.StatusNoContent)
}
