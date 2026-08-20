package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/miniagent"
)

func TestStreamClient_StreamClientNoTimeout(t *testing.T) {
	c := &StreamClient{APIKey: "sk", ChatURL: "http://x"}
	sc := c.streamClient()
	if sc.Timeout != 0 {
		t.Errorf("stream client Timeout = %v, want 0", sc.Timeout)
	}
	if c.streamClient() != sc {
		t.Error("stream client not cached")
	}
	// Simulate a fresh buildLLM injection: a client with a 120s overall Timeout + custom Transport.
	inj := &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{}}
	c2 := &StreamClient{APIKey: "sk", ChatURL: "http://x", HTTP: inj}
	got := c2.streamClient()
	if got.Timeout != 0 {
		t.Errorf("stream client Timeout = %v, want 0 (streaming zeroes out the overall Timeout)", got.Timeout)
	}
	if got.Transport != inj.Transport {
		t.Error("stream client must preserve the injected Transport (proxy/connection config)")
	}
}

// Run + Stream end-to-end: cfg.Stream=true goes through DoStream, OnDelta pushes deltas, Text is aggregated.
func TestRun_StreamAggregates(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"streamed"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`
	tr := &fakeTransport{responses: []string{sse}}
	chat, stream := testClients(tr)
	var deltas []string
	hooks := miniagent.LoopHooks{OnDelta: func(step int, kind miniagent.DeltaKind, text string) error {
		deltas = append(deltas, text)
		return nil
	}}
	res, err := miniagent.Run(context.Background(), &Provider{Chat: chat, Stream: stream}, miniagent.LoopConfig{Model: "m", Stream: true}, "hi", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "streamed" {
		t.Errorf("Text = %q", res.Text)
	}
	if len(deltas) != 1 || deltas[0] != "streamed" {
		t.Errorf("deltas = %v", deltas)
	}
}

// parseSSE reports an error on a malformed JSON chunk (mid-stream break / garbled data).
func TestParseSSE_MalformedChunk(t *testing.T) {
	sse := "data: not-json\n\ndata: [DONE]\n"
	if _, err := parseSSE(strings.NewReader(sse), nil); err == nil {
		t.Error("malformed chunk should error")
	}
}

// DoStream carries the custom request headers.
func TestDoStream_CustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "stream-val" {
			t.Errorf("X-Custom = %q, want stream-val", r.Header.Get("X-Custom"))
		}
		if r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL, Headers: map[string]string{"X-Custom": "stream-val"}}
	var deltas []miniagent.Delta
	resp, err := llm.DoStream(context.Background(), miniagent.Request{Model: "m"}, func(d miniagent.Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "hi" {
		t.Errorf("Text = %q", resp.Text)
	}
	if len(deltas) != 1 || deltas[0].Text != "hi" {
		t.Errorf("deltas = %+v", deltas)
	}
}
