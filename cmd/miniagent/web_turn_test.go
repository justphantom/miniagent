package main

// web_turn_test.go drives POST /api/turn end-to-end through the real turn engine with an
// injected fake LLM (no network): NDJSON streaming, session creation/persistence, resume,
// per-session serialization, and the sessions listing/replay endpoints.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
)

// fakeLLM answers every Do with fixed text; DoStream delegates to Do (no deltas — config
// defaults to non-stream). Records calls for assertions.
type fakeLLM struct {
	mu     sync.Mutex
	calls  int
	system string
}

func (f *fakeLLM) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	f.mu.Lock()
	f.calls++
	f.system = req.System
	f.mu.Unlock()
	return miniagent.Response{Text: "hello from fake", FinishReason: "stop", Usage: miniagent.Usage{InputTokens: 5, OutputTokens: 7}}, nil
}

func (f *fakeLLM) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	return f.Do(ctx, req)
}

func newTurnTestServer(t *testing.T) (*webServer, *fakeLLM) {
	t.Helper()
	return newTurnTestServerOn(context.Background(), t)
}

// newTurnTestServerOn builds a turn server whose baseCtx the test controls (shutdown tests).
func newTurnTestServerOn(baseCtx context.Context, t *testing.T) (*webServer, *fakeLLM) {
	t.Helper()
	fake := &fakeLLM{}
	dir := t.TempDir()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "p", ChatURL: "http://127.0.0.1:1/v1/chat/completions", Key: "sk", Models: []config.ModelConfig{{Name: "m"}}}},
		Defaults:  config.DefaultsConfig{Provider: "p", Model: "m"},
		Session:   config.SessionConfig{Dir: dir},
	}
	engine := &turnEngine{
		cfg:    cfg,
		logger: testLogger(),
		buildClients: func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
			return fake, nil, nil
		},
	}
	return newWebServer(baseCtx, cfg, engine, "", testLogger()), fake
}

func postTurn(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebTurn_CreatesSessionAndStreamsResult(t *testing.T) {
	s, fake := newTurnTestServer(t)
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Errorf("content-type = %q", ct)
	}
	var sessionID string
	var sawResult bool
	for line := range strings.SplitSeq(strings.TrimSpace(rec.Body.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad ndjson line %q: %v", line, err)
		}
		switch ev["type"] {
		case "session":
			sessionID, _ = ev["id"].(string)
		case "result":
			sawResult = true
			if ev["text"] != "hello from fake" {
				t.Errorf("result text = %v", ev["text"])
			}
		}
	}
	if sessionID == "" || !sawResult {
		t.Fatalf("missing session/result events: %s", rec.Body.String())
	}
	if fake.calls == 0 {
		t.Error("fake LLM was not called")
	}
	// Session jsonl persisted with meta + user + assistant lines.
	sessPath := filepath.Join(s.cfg.Session.Dir, sessionID+".jsonl")
	if _, err := os.Stat(sessPath); err != nil {
		t.Fatalf("session file not persisted: %v", err)
	}
	data, _ := os.ReadFile(sessPath)
	if got := strings.Count(string(data), "\n"); got < 3 {
		t.Errorf("session file lines = %d, want ≥3 (meta/user/assistant): %q", got, data)
	}
}

func TestWebTurn_ValidationErrors(t *testing.T) {
	s, _ := newTurnTestServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty prompt", `{"prompt":"","workdir":"/tmp"}`, http.StatusBadRequest},
		{"missing workdir", `{"prompt":"hi"}`, http.StatusBadRequest},
		{"relative workdir", `{"prompt":"hi","workdir":"rel/path"}`, http.StatusBadRequest},
		{"unknown field", `{"prompt":"hi","workdir":"/tmp","oops":1}`, http.StatusBadRequest},
		{"bad json", `{not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := postTurn(t, s.mux(), c.body)
		if rec.Code != c.want {
			t.Errorf("%s: code = %d, want %d, body = %s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

func TestWebTurn_ResumeSession(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"one","workdir":%q}`, workdir))
	var sessionID string
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["type"] == "session" {
			sessionID, _ = ev["id"].(string)
		}
	}
	if sessionID == "" {
		t.Fatal("no session id")
	}
	rec2 := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"two","workdir":%q,"session":%q}`, workdir, sessionID))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume code = %d: %s", rec2.Code, rec2.Body.String())
	}
	// Resumed session reuses the same file (no second session event — resume emits none).
	if strings.Contains(rec2.Body.String(), `"type":"session"`) {
		t.Error("resume emitted a session event, want none")
	}
	data, _ := os.ReadFile(filepath.Join(s.cfg.Session.Dir, sessionID+".jsonl"))
	if got := strings.Count(string(data), `"role":"user"`); got != 2 {
		t.Errorf("user turns in file = %d, want 2", got)
	}
}

func TestWebSessions_ListAndReplay(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	var sessionID string
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["type"] == "session" {
			sessionID, _ = ev["id"].(string)
		}
	}
	h := s.mux()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
	lr := httptest.NewRecorder()
	h.ServeHTTP(lr, req)
	var list []sessionSummary
	if err := json.Unmarshal(lr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list decode: %v (%s)", err, lr.Body.String())
	}
	if len(list) != 1 || list[0].ID != sessionID {
		t.Fatalf("list = %+v", list)
	}
	// Meta line fields must survive the listing: meta.Model is the modelSpec "provider/model";
	// workdir must be present so the UI can switch the active workdir when opening the session.
	if list[0].Provider != "p" || list[0].Model != "p/m" || list[0].Created == "" {
		t.Errorf("list meta = %+v, want provider p model p/m with created", list[0])
	}
	if list[0].Workdir != workdir {
		t.Errorf("list workdir = %q, want %q", list[0].Workdir, workdir)
	}

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+sessionID, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req2)
	if rr.Code != http.StatusOK {
		t.Fatalf("replay code = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`"type":"session"`, `"type":"text_delta"`, `"type":"result"`} {
		if !strings.Contains(body, want) {
			t.Errorf("replay missing %s: %s", want, body)
		}
	}

	req3 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/nope", nil)
	nr := httptest.NewRecorder()
	h.ServeHTTP(nr, req3)
	if nr.Code != http.StatusNotFound {
		t.Errorf("missing session code = %d, want 404", nr.Code)
	}
}

// Same-session concurrent turns collide: N7 replaced the blocking mutex wait with TryLock, so
// the second request answers 409 ("turn in progress") instead of hanging for up to the 10min web
// turn timeout with no feedback. One turn must reach the LLM at a time (RewriteMessages races would
// corrupt the shared file otherwise).
func TestWebTurn_SameSessionSerialized(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"one","workdir":%q}`, workdir))
	var sessionID string
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["type"] == "session" {
			sessionID, _ = ev["id"].(string)
		}
	}

	var mu sync.Mutex
	concurrent, maxConcurrent := 0, 0
	release := make(chan struct{})
	slow := &countingLLM{onCall: func() {
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()
		<-release // hold the turn open so the second request is guaranteed to collide
		mu.Lock()
		concurrent--
		mu.Unlock()
	}}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return slow, nil, nil
	}

	h := s.mux()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		body := fmt.Sprintf(`{"prompt":"p","workdir":%q,"session":%q}`, workdir, sessionID)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
	}()
	// Wait until the first turn has taken the session lock and entered the LLM.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		c := concurrent
		mu.Unlock()
		if c >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first turn never reached the LLM")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Second concurrent request on the same session → 409, no hang.
	rec2 := postTurn(t, h, fmt.Sprintf(`{"prompt":"p","workdir":%q,"session":%q}`, workdir, sessionID))
	if rec2.Code != http.StatusConflict {
		t.Errorf("concurrent turn code = %d, want 409: %s", rec2.Code, rec2.Body.String())
	}
	close(release)
	<-firstDone
	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent > 1 {
		t.Errorf("max concurrent LLM calls on one session = %d, want 1", maxConcurrent)
	}
}

type countingLLM struct{ onCall func() }

func (c *countingLLM) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	if c.onCall != nil {
		c.onCall()
	}
	return miniagent.Response{Text: "ok", FinishReason: "stop"}, nil
}

func (c *countingLLM) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	return c.Do(ctx, req)
}

// blockLLM holds every call until release closes, then answers — the hook for
// disconnect/stop tests that need a turn observably in flight.
type blockLLM struct {
	entered chan struct{} // closed on the first Do call
	release chan struct{}
	once    sync.Once
}

func (b *blockLLM) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return miniagent.Response{Text: "done", FinishReason: "stop"}, nil
	case <-ctx.Done():
		return miniagent.Response{}, ctx.Err()
	}
}

func (b *blockLLM) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	return b.Do(ctx, req)
}

// awaitLLM blocks until the LLM's first call (bounded).
func awaitLLM(t *testing.T, b *blockLLM) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never reached the LLM")
	}
}

// D1: the turn context derives from the server lifetime, not the request. Canceling the
// REQUEST context (client disconnect / tab close / view switch) must not cancel the agent —
// the session file still gets the full turn.
func TestWebTurn_DisconnectDoesNotKillTurn(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir)))
	req.Header.Set("Content-Type", "application/json")
	handlerDone := make(chan struct{})
	go func() { defer close(handlerDone); s.mux().ServeHTTP(httptest.NewRecorder(), req) }()

	awaitLLM(t, blocked)
	cancel() // client vanishes mid-turn
	<-handlerDone

	close(blocked.release) // let the agent finish
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(s.cfg.Session.Dir)
		if len(entries) > 0 {
			break // the turn completed and persisted despite the disconnect
		}
		if time.Now().After(deadline) {
			t.Fatal("session file never persisted after client disconnect")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// D1's stop side: POST /api/sessions/{id}/stop cancels the in-flight turn; the executed part
// is still saved (the partial-persist path), and the slot is released for a follow-up turn.
func TestWebTurn_StopAPI(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir)))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(httptest.NewRecorder(), req)
	}()
	awaitLLM(t, blocked)

	// No turn on an unrelated session → 404.
	rec404 := httptest.NewRecorder()
	s.mux().ServeHTTP(rec404, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/sessions/none-such/stop", nil))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("stop unknown session code = %d, want 404", rec404.Code)
	}

	id := onlyRunningSession(t, s)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/sessions/"+id+"/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop code = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	<-handlerDone // the canceled turn unwinds and saves

	// The follow-up turn shares the injected LLM — release it so the resume can complete.
	close(blocked.release)
	// Slot released: a new turn on the same session is accepted (not 409).
	rec2 := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"again","workdir":%q,"session":%q}`, workdir, id))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume after stop code = %d: %s", rec2.Code, rec2.Body.String())
	}
}

// onlyRunningSession returns the single running registry entry's id.
func onlyRunningSession(t *testing.T, s *webServer) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.turns.mu.Lock()
		id := ""
		n := 0
		for k, e := range s.turns.m {
			if e.kind == "running" {
				id, n = k, n+1
			}
		}
		s.turns.mu.Unlock()
		if n == 1 {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected exactly 1 running entry, got %d", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Two NEW sessions started concurrently must both run — the old "__new__" global lock
// serialized them; pre-generated ids key the registry by the real session id.
func TestWebTurn_ConcurrentNewSessions(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}

	done := make(chan int, 2)
	for range 2 {
		go func() {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.mux().ServeHTTP(rec, req)
			done <- rec.Code
		}()
	}
	awaitLLM(t, blocked)
	close(blocked.release)
	for i := range 2 {
		if code := <-done; code != http.StatusOK {
			t.Errorf("concurrent new turn %d code = %d, want 200", i, code)
		}
	}
	entries, _ := os.ReadDir(s.cfg.Session.Dir)
	if len(entries) != 2 {
		t.Errorf("persisted sessions = %d, want 2", len(entries))
	}
}

// web.max_concurrent_turns: overflowing requests answer 429; finishing a turn frees a slot.
func TestWebTurn_ConcurrencyCap(t *testing.T) {
	s, _ := newTurnTestServer(t)
	s.turnSem = make(chan struct{}, 1)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"one","workdir":%q}`, workdir)))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(httptest.NewRecorder(), req)
	}()
	awaitLLM(t, blocked)
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"two","workdir":%q}`, workdir))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("overflow code = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	close(blocked.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"three","workdir":%q}`, workdir))
		if rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed after turn finished (last code %d)", rec.Code)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Server shutdown (baseCtx cancel) cancels in-flight turns — the bounded-lifetime guarantee
// that keeps a turn from outliving the server process.
func TestWebTurn_ServerShutdownCancelsTurn(t *testing.T) {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s, _ := newTurnTestServerOn(baseCtx, t)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}
	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir)))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(httptest.NewRecorder(), req)
	}()
	awaitLLM(t, blocked)
	baseCancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.turns.mu.Lock()
		n := len(s.turns.m)
		s.turns.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight turn never unwound after baseCtx cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
