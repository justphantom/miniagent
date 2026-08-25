package main

// web_sessions_remote.go holds the minisession branches of the session handlers in
// web_sessions.go (list / delete; replay's branch is inline because its tail-cap + NDJSON
// streaming is shared with the local path). Split out to keep both files under the 300-line
// ceiling; behavior contract is documented in MINISESSION_INTEGRATION.md §4.

import (
	"errors"
	"net/http"
	"os"
	"sort"

	"github.com/justphantom/miniagent/miniagent/session"
)

// listSessionsRemote is the minisession branch of the listing: summaries come from the server
// (already "2006-01-02 15:04" strings — same shape as the local ModTime format, so the shared
// lexicographic sort stays chronological). Workdir is not part of the server summary (detail
// only): the field stays empty here and the UI backfills it from the session event when the
// session is opened (accepted degradation). running stays registry-local — cross-process
// running state is invisible by design. A transport error is 500, matching the local unreadable
// dir: a misconfigured session.url must not be masked by an empty list.
func (s *webServer) listSessionsRemote(w http.ResponseWriter, r *http.Request, remote *session.Client) {
	items, err := remote.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list remote sessions: " + err.Error()})
		return
	}
	out := make([]sessionSummary, 0, len(items))
	for _, it := range items {
		summary := sessionSummary{
			ID: it.ID, Provider: it.Provider, Model: it.Model,
			Created: it.Created, Size: it.Size, Modified: it.Modified, Preview: it.Preview,
		}
		if _, running := s.turns.running(it.ID); running {
			summary.Running = true
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Modified > out[j].Modified
	})
	writeJSON(w, http.StatusOK, out)
}

// deleteSessionRemote is the minisession branch of the delete: same beginDelete guard (an
// in-flight local writer must not resurrect the remote session via its next Rewrite), ErrNotExist
// → 404 like the local os.Remove path. The local <id>.tool-output cleanup is skipped: remote-mode
// tool output lives under each workdir (.miniagent/tool-output/{id}), whose location the server
// does not know — retention sweeps it (accepted over an extra detail round-trip).
func (s *webServer) deleteSessionRemote(w http.ResponseWriter, r *http.Request, id string, remote *session.Client) {
	if s.turns.beginDelete(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session has a turn in progress"})
		return
	}
	defer s.turns.finish(id, nil)
	if err := remote.DeleteSession(r.Context(), id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.turns.broadcastLife(lifeEvent("session_deleted", id, ""))
	w.WriteHeader(http.StatusNoContent)
}
