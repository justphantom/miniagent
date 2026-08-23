package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

func TestDoStream_Aggregates(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: {"usage":{"prompt_tokens":3,"completion_tokens":1}}
data: [DONE]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	var deltas []miniagent.Delta
	resp, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, func(d miniagent.Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "Hi" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if len(deltas) != 1 || deltas[0].Kind != miniagent.DeltaText || deltas[0].Text != "Hi" {
		t.Errorf("deltas = %+v", deltas)
	}
}

// DoStream non-200: in the pre-delta phase retries maxRetries times then still fails and propagates (contains "503").
// Retry-After: 0 makes backoff immediate to avoid test waits (strict assertions for the retry-exhaust path are in client_retry_test.go).
func TestDoStream_NonOKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "busy")
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	_, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want 503", err)
	}
}

// P3: DoStream receives context-length 400 when attempt>0, the error carries the "after N retries" prefix (consistent with the
// generic non-200 path, for debugging); the sentinel ErrContextLength chain can still be matched via errors.Is for Run to downgrade.
// Flow: first 503 triggers one retry → second call returns 400 context-length (at attempt=1, i.e. after 1 retries).
func TestDoStream_ContextLengthAfterRetry(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "busy")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens."}}`)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	_, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, miniagent.ErrContextLength) {
		t.Errorf("err = %v, want ErrContextLength in chain", err)
	}
	if !strings.Contains(err.Error(), "after 1 retries") {
		t.Errorf("err = %q, want \"after 1 retries\" prefix", err.Error())
	}
}

// P1-2: stream starts directly with [DONE] / no choices / only usage must error instead of returning an empty Response pretending success.
func TestDoStream_ErrorOmitsBody(t *testing.T) {
	const secret = "Bearer sk-leak-credential"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest) // non-context-length/thinking feature → take the generic status-only branch
		fmt.Fprint(w, `{"echoed":"`+secret+`"}`)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	_, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error must not echo response body (prevent credential leak into session): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain status code 400 for debugging: %q", err.Error())
	}
}

// Connection-drop detection (Fix 1): content streamed but the stream ended with NEITHER [DONE] NOR finish_reason
// (proxy/LB closed mid-generation). Without the fix the partial "Hello" is returned as a silent success (loop.go → FinishStop).
func TestDoStream_AllowUnterminatedAcceptsPartial(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hello partial"}}]}
` // content then EOF — no [DONE], no finish_reason
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL, StreamAllowUnterminated: true}
	resp, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("DoStream with StreamAllowUnterminated: %v", err)
	}
	if resp.Text != "Hello partial" {
		t.Errorf("Text = %q, want the partial content", resp.Text)
	}
}

// DoStream transparently retries a pre-delta stream end (Fix 2): first attempt returns 200 then immediate EOF (LB/proxy
// first-byte reset) — nothing reached the caller, so the retry duplicates zero live output. Second attempt succeeds.
func TestDoStream_PreDeltaResetRetried(t *testing.T) {
	const good = `data: {"choices":[{"delta":{"content":"Hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			return // 200 then immediate EOF (first-byte reset), no SSE data
		}
		fmt.Fprint(w, good)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	resp, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("DoStream: %v (expected transparent retry to succeed)", err)
	}
	if resp.Text != "Hi" {
		t.Errorf("Text = %q, want Hi", resp.Text)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (first EOFd pre-delta, retried)", attempts)
	}
}

// The 429 tpm-exhausted incident: the upstream reset the SSE stream mid-data-line, so the received chunk is truncated
// ("unexpected end of JSON input") before any delta reached the caller. isTransientStreamError classifies it as transient,
// so DoStream transparently retries (deltaSent==0 → zero live output duplicated) and the turn survives instead of aborting.
func TestDoStream_TruncatedChunkPreDeltaRetried(t *testing.T) {
	const good = `data: {"choices":[{"delta":{"content":"Hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Hi"`) // truncated JSON chunk, then EOF
			return
		}
		fmt.Fprint(w, good)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	resp, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("DoStream: %v (expected transparent retry of a truncated chunk to succeed)", err)
	}
	if resp.Text != "Hi" {
		t.Errorf("Text = %q, want Hi", resp.Text)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (first chunk truncated pre-delta, retried)", attempts)
	}
}

// Delta-lost incident variant: a complete content chunk already reached the caller (deltaSent>0, stream irrevocable
// per P2-4) before the upstream reset mid-data-line. The partial answer must NOT be thrown away and the turn must not
// fail: parseSSE returns the aggregated partial alongside the error, and DoStream accepts it as a truncated success
// (FinishReason=length → core truncation warning path) instead of replaying a half-stream.
func TestDoStream_TruncatedChunkPostDelta_ReturnsPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Hello "}}]}`+"\n"+
			`data: {"choices":[{"delta":{"content":"wor`) // complete chunk, then truncated JSON data line
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	resp, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("DoStream: %v (expected partial accepted as truncated success)", err)
	}
	if resp.Text != "Hello " {
		t.Errorf("Text = %q, want the streamed partial \"Hello \"", resp.Text)
	}
	if resp.FinishReason != "length" {
		t.Errorf("FinishReason = %q, want length (truncation marker for the core warning path)", resp.FinishReason)
	}
	if !resp.TruncatedStream {
		t.Error("TruncatedStream = false, want true (R4: the result event must flag the upstream cut so consumers can tell it from a natural finish)")
	}
}

// parseSSE surfaces the aggregated partial Response alongside the chunk-parse error (not an empty Response),
// so the caller can distinguish "partial content then abort" from "nothing before abort".
