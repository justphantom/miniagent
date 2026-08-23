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

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
)

// stepLLM answers 1st call with a tool call, subsequent calls with "done" — used by
// step_usage and tool_events tests.
type stepLLM struct {
	mu    sync.Mutex
	calls int
}

func (f *stepLLM) Do(_ context.Context, _ miniagent.Request) (miniagent.Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == 1 {
		return miniagent.Response{
			ToolCalls:    []miniagent.ToolCall{{ID: "c1", Name: "read", Args: `{"path":"nonexistent"}`}},
			FinishReason: "tool_calls",
			Usage:        miniagent.Usage{InputTokens: 100, OutputTokens: 20},
		}, nil
	}
	return miniagent.Response{
		Text:         "done",
		FinishReason: "stop",
		Usage:        miniagent.Usage{InputTokens: 50, OutputTokens: 10},
	}, nil
}

func (f *stepLLM) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	return f.Do(ctx, req)
}

func TestWebTurn_StopEmitsStopEvent(t *testing.T) {
	s, _ := newTurnTestServer(t)
	workdir := t.TempDir()
	blocked := &blockLLM{entered: make(chan struct{}), release: make(chan struct{})}
	s.engine.buildClients = func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return blocked, nil, nil
	}
	rec := newSyncRecorder()
	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/turn", strings.NewReader(fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir)))
		req.Header.Set("Content-Type", "application/json")
		s.mux().ServeHTTP(rec, req)
	}()
	awaitLLM(t, blocked)

	id := onlyRunningSession(t, s)
	stopRec := httptest.NewRecorder()
	s.mux().ServeHTTP(stopRec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/sessions/"+id+"/stop", nil))
	if stopRec.Code != http.StatusNoContent {
		t.Fatalf("stop code = %d: %s", stopRec.Code, stopRec.Body.String())
	}

	waitForBody(t, rec, `"type":"stop"`)
	body := rec.String()
	if strings.Contains(body, `"type":"error"`) {
		t.Errorf("stopped turn must not emit error: %s", body)
	}
	if strings.Contains(body, `"type":"result"`) {
		t.Errorf("stopped turn must not emit result: %s", body)
	}
}

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

func TestWebTurn_StepUsageEvents(t *testing.T) {
	s, _ := newTurnTestServer(t)
	sll := &stepLLM{}
	s.engine.buildClients = func(_ *config.Resolved, _ string, _ *slog.Logger, _ *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return sll, nil, nil
	}
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	var stepUsages []map[string]any
	var resultEv map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(rec.Body.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad ndjson: %v", err)
		}
		if ev["type"] == "step_usage" {
			stepUsages = append(stepUsages, ev)
		}
		if ev["type"] == "result" {
			resultEv = ev
		}
	}
	if len(stepUsages) < 1 {
		t.Fatalf("no step_usage events: %s", rec.Body.String())
	}
	// step increments from 1
	for i, su := range stepUsages {
		step, _ := su["step"].(float64)
		if int(step) != i+1 {
			t.Errorf("step_usage[%d].step = %v, want %d", i, step, i+1)
		}
	}
	// sum matches result
	var sumIn, sumOut int
	for _, su := range stepUsages {
		sumIn += int(su["input_tokens"].(float64))
		sumOut += int(su["output_tokens"].(float64))
	}
	resIn := int(resultEv["input_tokens"].(float64))
	resOut := int(resultEv["output_tokens"].(float64))
	if sumIn != resIn {
		t.Errorf("sum input_tokens = %d, result = %d", sumIn, resIn)
	}
	if sumOut != resOut {
		t.Errorf("sum output_tokens = %d, result = %d", sumOut, resOut)
	}
}

func TestWebTurn_ToolEventsCarryStep(t *testing.T) {
	s, _ := newTurnTestServer(t)
	sll := &stepLLM{}
	s.engine.buildClients = func(_ *config.Resolved, _ string, _ *slog.Logger, _ *transportCache) (miniagent.LLM, miniagent.Doer, error) {
		return sll, nil, nil
	}
	workdir := t.TempDir()
	rec := postTurn(t, s.mux(), fmt.Sprintf(`{"prompt":"hi","workdir":%q}`, workdir))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	var toolUseSteps, toolResultSteps []int
	for line := range strings.SplitSeq(strings.TrimSpace(rec.Body.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad ndjson: %v", err)
		}
		if ev["type"] == "tool_use" {
			toolUseSteps = append(toolUseSteps, int(ev["step"].(float64)))
		}
		if ev["type"] == "tool_result" {
			toolResultSteps = append(toolResultSteps, int(ev["step"].(float64)))
		}
	}
	if len(toolUseSteps) != 1 || toolUseSteps[0] != 1 {
		t.Errorf("tool_use steps = %v, want [1]", toolUseSteps)
	}
	if len(toolResultSteps) != 1 || toolResultSteps[0] != 1 {
		t.Errorf("tool_result steps = %v, want [1]", toolResultSteps)
	}
}
