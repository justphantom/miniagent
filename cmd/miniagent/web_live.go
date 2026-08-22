package main

// web_live.go serves the two sync streams of the multi-browser story:
//
//   - GET /api/events: the global lifecycle feed (turn_started / turn_finished /
//     session_deleted / ping). Every open browser keeps one of these and refreshes its
//     session list / running dots from it.
//   - GET /api/sessions/{id}/live: attach to a session's in-flight turn — the replay buffer
//     from event zero, then live lines until the turn ends (live_end). A browser that gets
//     409 on POST /api/turn upgrades into this stream and watches the running turn instead.
//
// Both are plain fetch NDJSON streams (not EventSource: it cannot carry the x-api-key header)
// sharing the frontend's existing reader loop.

import (
	"net/http"
	"time"

	"github.com/justphantom/miniagent/miniagent/session"
)

// lifePingInterval spaces keepalive pings on the global stream: proxies with idle
// timeouts (nginx default 60s) would otherwise silently drop the connection.
const lifePingInterval = 15 * time.Second

func (s *webServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	ch := s.turns.subscribeLife()
	defer s.turns.unsubscribeLife(ch)

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	nw := &ndjsonWriter{w: w, f: flusherOf(w)}
	if nw.WriteLine("{\"type\":\"hello\"}\n") != nil {
		return
	}

	ticker := time.NewTicker(lifePingInterval)
	defer ticker.Stop()
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return // evicted (stalled) — the client reconnects and re-syncs
			}
			if nw.WriteLine(line) != nil {
				return
			}
		case <-ticker.C:
			if nw.WriteLine("{\"type\":\"ping\"}\n") != nil {
				return
			}
		case <-r.Context().Done():
			return // browser closed the page — no turn is affected (D1)
		case <-s.baseCtx.Done():
			return // server shutting down
		}
	}
}

// handleSessionLive streams the session's in-flight turn from event zero (plus a
// live_truncated marker when the buffer overflowed), then follows live output until the turn
// ends. No in-flight turn → a single live_end line so the client can fall back to replay.
func (s *webServer) handleSessionLive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := session.ValidateSessionID(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	entry, ok := s.turns.running(id)
	if !ok {
		s.streamLiveEnd(w)
		return
	}
	replay, ch, ok := entry.subscribe()
	if !ok { // finished between the running check and the subscribe
		s.streamLiveEnd(w)
		return
	}
	defer entry.unsubscribe(ch)

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	nw := &ndjsonWriter{w: w, f: flusherOf(w)}
	for _, line := range replay {
		if nw.WriteLine(line) != nil {
			return
		}
	}
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				_ = nw.WriteLine("{\"type\":\"live_end\"}\n") // clean end-of-turn marker
				return
			}
			if nw.WriteLine(line) != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.baseCtx.Done():
			return
		}
	}
}

func (s *webServer) streamLiveEnd(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("{\"type\":\"live_end\"}\n"))
}
