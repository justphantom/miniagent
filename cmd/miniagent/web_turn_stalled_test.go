package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
)

// stalledWriter blocks every Write until release — the network-shaped stand-in for a slow
// subscriber (httptest's in-memory recorder never blocks, which is exactly why the lag-cut
// path was invisible to the existing suite).
type stalledWriter struct {
	release chan struct{}
}

func (s *stalledWriter) Header() http.Header { return http.Header{} }
func (s *stalledWriter) Write(p []byte) (int, error) {
	<-s.release
	return len(p), nil
}
func (s *stalledWriter) WriteHeader(int) {}

func TestWebTurn_LagCutSignalsStreamCut(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}
	stalled := &stalledWriter{release: make(chan struct{})}
	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir)))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(stalled, req)
	}()
	awaitLLM(t, blocked)

	// Overrun the subscriber buffer while its handler is stalled on the first write.
	sessionLine := `{"type":"session"}` + "\n"
	for range turnSubchanBuffer + 10 {
		s.turns.mu.Lock()
		for _, e := range s.turns.m {
			_, _ = e.Write([]byte(sessionLine))
		}
		s.turns.mu.Unlock()
	}
	close(stalled.release) // unstick the writer — the closed channel surfaces as stream_cut

	id := onlyRunningSession(t, s)
	liveRec := newSyncRecorder()
	go func() {
		s.mux().ServeHTTP(liveRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+id+"/live", nil))
	}()
	waitForBody(t, liveRec, `"type":"session"`)

	close(blocked.release) // finish the turn
	waitForBody(t, liveRec, `"type":"result"`)
	waitForBody(t, liveRec, `"type":"live_end"`)
	if body := liveRec.String(); strings.Contains(body, `"type":"stream_cut"`) {
		t.Errorf("healthy live subscriber must not be cut: %s", body)
	}
}

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
