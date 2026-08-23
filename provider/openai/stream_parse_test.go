package openai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

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
func TestParseSSE_TruncatedNoDoneNoFinish_Errors(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hello"}}]}
` // reader EOF after content — no finish_reason, no [DONE]
	_, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("expected error for content-then-EOF with no [DONE]/finish_reason, got nil")
	}
	if !strings.Contains(err.Error(), "[DONE]") && !strings.Contains(err.Error(), "finish_reason") {
		t.Errorf("error should mention [DONE]/finish_reason, got: %v", err)
	}
}

// OR-of-markers (Fix 1): a stream that emits finish_reason but NEVER [DONE] (Azure / some LiteLLM configs) still succeeds.
func TestParseSSE_FinishReasonOnlyNoDone_OK(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
` // no [DONE], but finish_reason present
	res, err := parseSSE(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("finish_reason-only stream should succeed: %v", err)
	}
	if res.Text != "Hi" || res.FinishReason != "stop" {
		t.Errorf("res = %+v", res)
	}
}

// Index collision without ids (Fix 3): a compat endpoint omitting index sends two distinct tool_calls both at index 0
// with no stable ids — the second (name="shell") colliding into the first (name="read") must error instead of silently
// overwriting the name and concatenating args.
func TestParseSSE_ToolCallIndexCollisionNameMismatch(t *testing.T) {
	const sse = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"shell","arguments":"{}"}}]}}]}` + "\n" +
		"data: [DONE]\n"
	_, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("expected index-collision error, got nil")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("error should mention collision, got: %v", err)
	}
}

// StreamAllowUnterminated (opt-in flag): a content-then-EOF stream (no [DONE]/finish_reason) is accepted as success with
// the partial Response — for non-compliant endpoints (vLLM/Ollama). Default (flag off) would surface errStreamUnterminated.
func TestParseSSE_TruncatedChunkReturnsPartial(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hello "}}]}` + "\n" +
		`data: {"choices":[{"delta":{"content":"wor` // truncated JSON
	res, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("expected parse error for truncated JSON chunk")
	}
	if !strings.Contains(err.Error(), "parse sse chunk") {
		t.Errorf("err = %v, want parse sse chunk wrap", err)
	}
	if res.Text != "Hello " {
		t.Errorf("Text = %q, want the pre-abort partial \"Hello \"", res.Text)
	}
}
