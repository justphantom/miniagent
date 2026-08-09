package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// Plain text + reasoning + usage + [DONE]: aggregation correct, onDelta receives each fragment.
func TestParseSSE_TextReasoningUsage(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hello "}}]}
data: {"choices":[{"delta":{"content":"world","reasoning_content":"think"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: {"usage":{"prompt_tokens":5,"completion_tokens":2}}
data: [DONE]
`
	var deltas []miniagent.Delta
	res, err := parseSSE(strings.NewReader(sse), func(d miniagent.Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if res.Text != "Hello world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Reasoning != "think" {
		t.Errorf("Reasoning = %q", res.Reasoning)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
	if res.Usage.InputTokens != 5 || res.Usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	// "Hello ", "world", "think": one delta each.
	if len(deltas) != 3 {
		t.Errorf("deltas = %d (%+v)", len(deltas), deltas)
	}
}

// tool_calls accumulated across multiple chunks by index, multiple indexes aggregated in ascending order.
func TestParseSSE_ToolCallsAccumulated(t *testing.T) {
	const sse = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"shell","arguments":"{}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n" +
		"data: [DONE]\n"
	res, err := parseSSE(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].ID != "call_1" || res.ToolCalls[0].Name != "read" || res.ToolCalls[0].Args != `{"path":"a.go"}` {
		t.Errorf("call0 = %+v", res.ToolCalls[0])
	}
	if res.ToolCalls[1].ID != "call_2" || res.ToolCalls[1].Name != "shell" || res.ToolCalls[1].Args != "{}" {
		t.Errorf("call1 = %+v", res.ToolCalls[1])
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
}

// DoStream fed SSE via httptest: aggregate Response + onDelta pushes increments.
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
func TestParseSSE_EmptyStreamErrors(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{"done-only", "data: [DONE]\n"},
		{"empty-input", ""},
		{"usage-only-no-choices", `data: {"usage":{"prompt_tokens":5,"completion_tokens":0}}` + "\ndata: [DONE]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSSE(strings.NewReader(tc.sse), nil)
			if err == nil {
				t.Fatalf("expected error for %s stream", tc.name)
			}
			if !strings.Contains(err.Error(), "without any choices") {
				t.Errorf("%s: err = %v", tc.name, err)
			}
		})
	}
}

// P1-3: provider reports an error mid-stream via a {"error":{"message":...}} chunk, must propagate instead of swallowing it as success.
func TestParseSSE_MidStreamError(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n" +
		`data: {"error":{"message":"content filter triggered"}}` + "\n" +
		"data: [DONE]\n"
	_, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("expected error for mid-stream error chunk")
	}
	if !strings.Contains(err.Error(), "content filter triggered") {
		t.Errorf("err should carry provider message: %v", err)
	}
	if !strings.Contains(err.Error(), "stream error from provider") {
		t.Errorf("err should be flagged as provider stream error: %v", err)
	}
}

// P3-2: a single data line payload > 1MB (old limit) but < 4MB (new limit) should parse, not trigger ErrTooLong.
func TestParseSSE_LongLine(t *testing.T) {
	big := strings.Repeat("a", 1500*1024)
	chunk := fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, big)
	sse := "data: " + chunk + "\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n" +
		"data: [DONE]\n"
	res, err := parseSSE(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("parseSSE long line: %v", err)
	}
	if len(res.Text) != len(big) {
		t.Errorf("Text len = %d, want %d", len(res.Text), len(big))
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
}

// P2-5/P1-A: when c.HTTP==nil, streamClient returns a client without Timeout (stream total duration is controlled by ctx;
// http.Client.Timeout covers body reading and would cut off long streams); caches the same instance; if injected, reuse the injected one.
// Injection contract (buildLLM injects a client with a 120s Timeout): streamClient must zero out the injected client's
// total Timeout (P1-A: streaming body must not be cut), but preserve its Transport (proxy/connection config, #2).

// DoStream non-200 error must not echo the response body: prevents malicious/debug proxies from echoing Authorization in the
// error body, which would let the key leak into NDJSON stdout / session jsonl via the error. Aligns with the threat model of the
// non-streaming client.go.
// Regression: previously three places used text.Truncate(raw,500,…) to dump the body into the error.
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
