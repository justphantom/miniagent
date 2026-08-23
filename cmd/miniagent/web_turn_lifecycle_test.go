package main

import (
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

// onlyRunningSession polls the turn registry until exactly one running entry appears.
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

	// The turn must still be in flight (D1): the handler's return must not have canceled
	// it (the bug was a handler-scoped defer cancel). A second window attaches live and
	// observes the turn run to completion after the block releases.
	id := onlyRunningSession(t, s)
	liveRec := newSyncRecorder()
	go func() {
		s.mux().ServeHTTP(liveRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+id+"/live", nil))
	}()
	waitForBody(t, liveRec, `"type":"session"`)

	close(blocked.release) // let the agent finish
	waitForBody(t, liveRec, `"type":"result"`)
	if body := liveRec.String(); strings.Contains(body, `"type":"stop"`) || strings.Contains(body, `"type":"error"`) {
		t.Errorf("turn after client disconnect ended abnormally: %s", body)
	}
}

func TestTurnEntry_LagClose(t *testing.T) {
	e := &turnEntry{kind: "running", id: "s-lag", subs: map[chan string]struct{}{}, done: make(chan struct{})}
	_, ch, ok := e.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	line := strings.Repeat("x", 32) + "\n"
	for range turnSubchanBuffer + 5 {
		if _, err := e.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// close(ch) does not discard buffered values — drain until the close surfaces.
drained:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drained
			}
		default:
			t.Fatal("channel drained without a close — lag close did not fire")
		}
	}
	select {
	case <-e.done:
		t.Fatal("done must NOT be closed — the turn is still running")
	default:
	}
	// The full replay buffer still holds every line (late rebuild via /live or replay).
	if len(e.buf) != turnSubchanBuffer+5 {
		t.Errorf("buf = %d lines, want %d (replay material must survive the cut)", len(e.buf), turnSubchanBuffer+5)
	}
}
