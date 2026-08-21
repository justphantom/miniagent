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
		buildClients: func(resolved *config.Resolved, apiKey string, logger *slog.Logger) (miniagent.LLM, miniagent.Doer, error) {
			return fake, nil, nil
		},
	}
	return &webServer{cfg: cfg, engine: engine, key: "", logger: testLogger()}, fake
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
		{"relative workdir", `{"prompt":"hi","workdir":"rel/path"}`, http.StatusInternalServerError},
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

// Same-session turns serialize: with a slow fake LLM, the second request must not start until
// the first finishes (RewriteMessages races would corrupt the shared file otherwise).
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
	slow := &countingLLM{onCall: func() {
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
	}}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger) (miniagent.LLM, miniagent.Doer, error) {
		return slow, nil, nil
	}

	h := s.mux()
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			body := fmt.Sprintf(`{"prompt":"p","workdir":%q,"session":%q}`, workdir, sessionID)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(body))
			r := httptest.NewRecorder()
			h.ServeHTTP(r, req)
			if r.Code != http.StatusOK {
				t.Errorf("turn code = %d: %s", r.Code, r.Body.String())
			}
		})
	}
	wg.Wait()
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
