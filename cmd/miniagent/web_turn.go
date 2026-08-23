package main

// web_turn.go handles POST /api/turn: parses the JSON request, reserves the session in the
// turn registry (same-session turns still answer 409), runs the turn decoupled from the
// request connection (D1: client disconnect/tab close does NOT cancel the agent — stopping is
// an explicit POST /api/sessions/{id}/stop), and streams the entry's events back to the
// caller as one subscriber among possibly many. The streamed contract stays byte-identical
// to the CLI stdout stream (L0 #12).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
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

// (s.baseCtx), NOT from r.Context() — a client disconnect must not cancel the agent.
//
//nolint:contextcheck // D1: the turn context deliberately derives from the server lifetime
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

	// New sessions get their id up front (generateSessionID is collision-free): the registry
	// key is always the real session id, so two concurrent NEW turns no longer serialize on a
	// global "__new__" slot — different sessions genuinely run in parallel.
	id := req.Session
	presetID := ""
	if id == "" {
		presetID = generateSessionID()
		id = presetID
	}

	// web.max_concurrent_turns gate (D2: default 0 = unlimited). Non-blocking acquire: a
	// full server answers 429 instead of queueing a request that would hold no feedback.
	if s.turnSem != nil {
		select {
		case s.turnSem <- struct{}{}:
			defer func() { <-s.turnSem }()
		default:
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent turns (web.max_concurrent_turns)"})
			return
		}
	}

	// The turn context derives from the server lifetime, not the request: a disconnect must
	// not kill the agent (D1). runTurn layers the config max_duration on top of this bound.
	// cancel is owned by the turn goroutine (deferred there): a handler-scoped defer would
	// fire when the subscriber loop returns on client disconnect and kill the live turn.
	const defaultWebMaxDuration = 10 * time.Minute
	turnCtx, cancel := context.WithTimeout(s.baseCtx, defaultWebMaxDuration)

	entry, busy := s.turns.register(id, cancel)
	if busy {
		cancel()
		// Same-session turn already running (N7 semantics kept): the UI can attach to the
		// running turn via GET /api/sessions/{id}/live instead of hanging or retrying.
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a turn on this session is already in progress"})
		return
	}
	s.turns.broadcastLife(lifeEvent("turn_started", id, ""))

	spec := turnSpec{
		prompt:        req.Prompt,
		workdir:       req.Workdir,
		sessionArg:    req.Session,
		sessionID:     presetID, // non-empty only for new sessions; resolveSession uses it verbatim
		saveNew:       req.Session == "",
		maxIterDef:    0, // web: rely on config run.max_iterations / builtin default
		emitStepUsage: true,
		overrides:     webOverrides(req),
	}

	go func() {
		defer cancel() // turn-scoped: releases the timeout context when the turn ends
		err := s.engine.runTurn(turnCtx, spec, entry)
		s.turns.finish(id, err)
		s.turns.broadcastLife(turnFinishedEvent(id, err))
	}()

	// This handler is now just one subscriber of the entry: the response mirrors what the
	// engine wrote, but the turn keeps running after the client is gone.
	replay, ch, ok := entry.subscribe()
	defer entry.unsubscribe(ch)
	if !ok { // turn finished before we subscribed (degenerate fast path)
		s.writeTurnError(w, entry.result())
		return
	}
	if len(replay) == 0 {
		// Hold the 200 until the first event: an error before any NDJSON byte (missing
		// resume session, bad config) must still answer structured JSON, as before.
		line, ok := <-ch
		if !ok {
			s.writeTurnError(w, entry.result())
			return
		}
		replay = append(replay, line)
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no") // reverse proxies (nginx) must not buffer the NDJSON stream
	nw := &ndjsonWriter{w: w, f: flusherOf(w)}

	for _, line := range replay {
		if nw.WriteLine(line) != nil {
			return // client gone mid-replay; turn continues on the bus
		}
	}
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return // turn finished: every buffered line has been drained
			}
			if nw.WriteLine(line) != nil {
				return
			}
		case <-r.Context().Done():
			return // client disconnected — the turn keeps running (D1)
		}
	}
}

// writeTurnError maps a turn error that produced no streamed events to a structured JSON
// response (the pre-stream error contract: 404 for a missing resume session, 500 otherwise;
// a stopped turn that streamed nothing is a clean 204).
func (s *webServer) writeTurnError(w http.ResponseWriter, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, errSessionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// flusherOf returns the ResponseWriter's Flusher when the transport supports it.
func flusherOf(w http.ResponseWriter) http.Flusher {
	f, _ := w.(http.Flusher)
	return f
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
