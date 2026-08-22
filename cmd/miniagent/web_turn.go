package main

// web_turn.go handles POST /api/turn: parses the JSON request, serializes turns on the same
// session file, and runs the turn engine with the per-request ResponseWriter as the NDJSON sink.
// The streamed contract is byte-identical to the CLI stdout stream (L0 #12), so the frontend
// reuses the same NDJSON parser logic.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/miniagent/config"
)

// webTurnRequest is the POST /api/turn body. Session is a resume id; when empty the server
// creates a new session (web turns are always persisted — the UI needs the session list to
// populate and the id is returned via the NDJSON session event).
type webTurnRequest struct {
	Prompt   string `json:"prompt"`
	Workdir  string `json:"workdir"`
	Session  string `json:"session"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

func (s *webServer) handleTurn(w http.ResponseWriter, r *http.Request) {
	// CSRF defense (decisive when auth is off): a cross-site form/no-cors fetch cannot carry
	// application/json — CORS-safelisted content types are text/plain, multipart and the
	// urlencoded forms. Rejecting anything else closes the forged-POST hole outright.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content-type must be application/json"})
		return
	}
	var req webTurnRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPromptBytes+(1<<16)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}
	if req.Workdir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workdir is required"})
		return
	}
	if !filepath.IsAbs(req.Workdir) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workdir must be an absolute path"})
		return
	}
	if int64(len(req.Prompt)) > maxPromptBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt exceeds the size limit"})
		return
	}

	// Serialize turns on the same session: RewriteMessages is atomic but concurrent in-process
	// turns would race the first meta line. New sessions have a fresh generated id (collision-free),
	// so the lock only matters for resume. The entry for an empty key serializes new-turn requests
	// globally — acceptable (they are interactive, not high-throughput).
	lockKey := req.Session
	if lockKey == "" {
		lockKey = "__new__"
	}
	mu, _ := s.locks.LoadOrStore(lockKey, &sync.Mutex{})
	if !mu.(*sync.Mutex).TryLock() {
		// N7: a same-session turn is already running (≤10min by the web default timeout) — the
		// client would otherwise hang with no feedback; answer 409 so the UI can say "turn in progress".
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a turn on this session is already in progress"})
		return
	}
	defer mu.(*sync.Mutex).Unlock()

	// Stream NDJSON: headers + a flush-wrapping writer. Once the first byte is written the status
	// is implicitly 200; errors after that point are appended as NDJSON error events by runTurn itself.
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no") // reverse proxies (nginx) must not buffer the NDJSON stream
	flusher, _ := w.(http.Flusher)
	nw := &ndjsonWriter{w: w, f: flusher}

	// L15: web turn default timeout. run.max_duration defaults to nil → if a tool hangs and
	// doesn't respect ctx, the session lock is held forever (409 on all subsequent turns).
	// Apply a conservative default so a stuck turn self-recovers. runTurn already layers the
	// config max_duration on top; a web default tighter than the config wins via ctx deadline.
	const defaultWebMaxDuration = 10 * time.Minute
	ctx, cancel := context.WithTimeout(r.Context(), defaultWebMaxDuration)
	defer cancel()

	spec := turnSpec{
		prompt:     req.Prompt,
		workdir:    req.Workdir,
		sessionArg: req.Session,
		saveNew:    req.Session == "", // empty session = create
		maxIterDef: 0,                 // web: rely on config run.max_iterations / builtin default
		overrides:  webOverrides(req),
	}
	err := s.engine.runTurn(ctx, spec, nw)
	if errors.Is(err, errSessionNotFound) {
		if nw.n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		return
	}
	if err != nil {
		if nw.n == 0 {
			// No event streamed yet → safe to answer with a structured JSON error.
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// Mid-turn failure: runTurn already emitted the error event; nothing else to do.
		return
	}
}

// webOverrides builds CLIOverrides from the request's optional provider/model/thinking fields.
// Only non-empty fields are set (nil = fall back to config defaults), matching the cli>config rule.
func webOverrides(req webTurnRequest) config.CLIOverrides {
	o := config.CLIOverrides{}
	if req.Provider != "" {
		p := req.Provider
		o.Provider = &p
	}
	if req.Model != "" {
		m := req.Model
		o.Model = &m
	}
	if req.Thinking != "" {
		t := req.Thinking
		o.Thinking = &t
	}
	return o
}
