package main

// web_live_test.go covers the sync streams: GET /api/events (lifecycle feed) and
// GET /api/sessions/{id}/live (mid-turn attach with replay + live_end), plus the running
// flag on the sessions listing. Streaming handlers write while the test asserts, so the
// recorder must be mutex-guarded (httptest.ResponseRecorder is not).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
)

// syncRecorder is a mutex-guarded ResponseWriter for reading a live stream's body while its
// handler goroutine keeps writing.
type syncRecorder struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	hdr  http.Header
	code int
}

func newSyncRecorder() *syncRecorder        { return &syncRecorder{hdr: make(http.Header)} }
func (r *syncRecorder) Header() http.Header { return r.hdr }
func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(p)
}
func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}
func (r *syncRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// waitForBody polls the recorder until want appears (streaming handlers deliver async).
func waitForBody(t *testing.T, rec *syncRecorder, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(rec.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("stream never delivered %q, got: %s", want, rec.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// blockedTurn injects a blockLLM and starts a POST /api/turn in the background against rec.
func blockedTurn(t *testing.T, s *webServer, rec http.ResponseWriter, body string) *blockLLM {
	t.Helper()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}
	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(rec, req)
	}()
	awaitLLM(t, blocked)
	return blocked
}

// /api/events: hello first, then turn_started for a new turn, turn_finished after it ends.
func TestWebEvents_Lifecycle(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()

	eventsRec := newSyncRecorder()
	go func() {
		s.mux().ServeHTTP(eventsRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/events", nil))
	}()
	waitForBody(t, eventsRec, `"type":"hello"`)

	blocked := blockedTurn(t, s, httptest.NewRecorder(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	waitForBody(t, eventsRec, `"type":"turn_started"`)
	close(blocked.release)
	waitForBody(t, eventsRec, `"type":"turn_finished"`)

	var fin struct {
		Type    string `json:"type"`
		Session string `json:"session"`
		OK      bool   `json:"ok"`
	}
	for line := range strings.SplitSeq(eventsRec.String(), "\n") {
		if strings.Contains(line, "turn_finished") {
			if err := json.Unmarshal([]byte(line), &fin); err != nil {
				t.Fatalf("turn_finished line: %v", err)
			}
		}
	}
	if fin.Type != "turn_finished" || !fin.OK || fin.Session == "" {
		t.Errorf("turn_finished = %+v, want ok=true with session id", fin)
	}
}

// /api/sessions/{id}/live with no turn in flight → a single live_end line.
func TestWebLive_NoTurn(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	var id string
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		var ev struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "session" {
			id = ev.ID
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+id+"/live", nil)
	lr := httptest.NewRecorder()
	s.mux().ServeHTTP(lr, req)
	if lr.Code != http.StatusOK {
		t.Fatalf("live code = %d", lr.Code)
	}
	if got := strings.TrimSpace(lr.Body.String()); got != `{"type":"live_end"}` {
		t.Errorf("live body = %q, want single live_end", got)
	}
}

// /api/sessions/{id}/live on a running turn: replays buffered events from zero, follows live
// output, ends with live_end when the turn finishes — a second browser sees what the
// originating one saw.
func TestWebLive_AttachMidTurn(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	blocked := blockedTurn(t, s, newSyncRecorder(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	id := onlyRunningSession(t, s)

	liveRec := newSyncRecorder()
	go func() {
		s.mux().ServeHTTP(liveRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+id+"/live", nil))
	}()
	waitForBody(t, liveRec, `"type":"session"`) // replay delivered from event zero

	close(blocked.release)
	waitForBody(t, liveRec, `"type":"result"`)
	waitForBody(t, liveRec, `"type":"live_end"`)
	if strings.Index(liveRec.String(), `"type":"live_end"`) < strings.Index(liveRec.String(), `"type":"result"`) {
		t.Errorf("live_end arrived before the result event: %s", liveRec.String())
	}
}

// The sessions list carries running=true while a turn is in flight on a persisted session,
// false after it ends.
func TestWebSessionsList_RunningFlag(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()

	// First (fast) turn persists the session file — a running new-session turn has no file row
	// yet (the jsonl lands only at turn end), so the flag is observable on the second turn.
	blocked := blockedTurn(t, s, httptest.NewRecorder(), fmt.Sprintf(`{"prompt":"one","workdir":%q}`, workdir))
	close(blocked.release)

	list := func() map[string]bool {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, req)
		var rows []sessionSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("list decode: %v", err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.ID] = r.Running
		}
		return out
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(list()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first session never persisted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	var id string
	for k := range list() {
		id = k
	}

	blocked2 := blockedTurn(t, s, httptest.NewRecorder(), fmt.Sprintf(`{"prompt":"two","workdir":%q,"session":%q}`, workdir, id))
	if !list()[id] {
		t.Error("running flag not set while a turn is in flight")
	}
	close(blocked2.release)
	deadline = time.Now().Add(5 * time.Second)
	for list()[id] {
		if time.Now().After(deadline) {
			t.Fatal("running flag never cleared after turn finished")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
