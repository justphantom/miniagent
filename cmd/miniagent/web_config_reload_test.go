package main

// web_config_reload_test.go covers POST /api/reload (service restart without process exit);
// GET/PUT /api/config tests live in web_config_test.go (split to stay under the 300-line ceiling).

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

func reloadReq() *http.Request {
	return httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/reload", nil)
}

func TestConfigReload_NoFileAnswers404(t *testing.T) {
	// Test servers carry no cfgPath: reload, like PUT, is disabled without a backing file.
	s, _ := newTurnTestServer(t)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, reloadReq())
	if rec.Code != http.StatusNotFound {
		t.Errorf("reload without cfgPath code = %d, want 404", rec.Code)
	}
}

func TestConfigReload_CorruptFileRejected(t *testing.T) {
	s, path := cfgTestServer(t)
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, reloadReq())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reload with corrupt file code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-s.reloadCh:
		t.Fatal("corrupt file must not signal reload")
	default:
	}
}

func TestConfigReload_RunningTurnRejected(t *testing.T) {
	s, _ := cfgTestServer(t)
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}
	go func() {
		body := fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, t.TempDir())
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(httptest.NewRecorder(), req)
	}()
	awaitLLM(t, blocked)
	defer close(blocked.release)

	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, reloadReq())
	if rec.Code != http.StatusConflict {
		t.Fatalf("reload with in-flight turn code = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-s.reloadCh:
		t.Fatal("in-flight turn must not signal reload")
	default:
	}
}

func TestConfigReload_IdleSignalsChannel(t *testing.T) {
	s, _ := cfgTestServer(t)
	ready := make(chan struct{})
	go func() { <-s.reloadCh; close(ready) }() // the serve loop's receiving half
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, reloadReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("idle reload code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("idle reload must signal the restart loop")
	}
}
