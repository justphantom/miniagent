package anthropic

import (
	"errors"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// sseStream joins event lines into a single SSE byte stream (each event is an event: line + a data: line).
func sseStream(events ...string) string {
	return strings.Join(events, "\n") + "\n"
}

func TestParseSSE_TextDeltaAggregatesAndEmits(t *testing.T) {
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":10}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	var deltas []miniagent.Delta
	res, err := parseSSE(strings.NewReader(stream), func(d miniagent.Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" {
		t.Errorf("Text = %q, want hello", res.Text)
	}
	if len(deltas) != 2 || deltas[0].Text != "hel" || deltas[1].Text != "lo" {
		t.Errorf("deltas = %v", deltas)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want in=10 out=5", res.Usage)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", res.FinishReason)
	}
}

func TestParseSSE_CacheTokensFoldedIntoInput(t *testing.T) {
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":100,"cache_creation_input_tokens":30,"cache_read_input_tokens":70}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	res, _ := parseSSE(strings.NewReader(stream), nil)
	if res.Usage.InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200 (100+30+70 folded)", res.Usage.InputTokens)
	}
}

func TestParseSSE_ThinkingDeltaEmitsReasoning(t *testing.T) {
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":1}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	var got miniagent.DeltaKind
	res, err := parseSSE(strings.NewReader(stream), func(d miniagent.Delta) error { got = d.Kind; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Reasoning != "pondering" {
		t.Errorf("Reasoning = %q", res.Reasoning)
	}
	if got != miniagent.DeltaReasoning {
		t.Errorf("delta kind = %v, want DeltaReasoning", got)
	}
}

func TestParseSSE_InputJSONDeltaAccumulatesToolUse(t *testing.T) {
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":1}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	res, err := parseSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "t1" || tc.Name != "f" || tc.Args != `{"a":1}` {
		t.Errorf("ToolCall = %+v", tc)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", res.FinishReason)
	}
}

func TestParseSSE_SignatureDeltaIgnored(t *testing.T) {
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":1}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	res, err := parseSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reasoning != "" {
		t.Errorf("Reasoning = %q, want empty (signature has no text consumer)", res.Reasoning)
	}
}

func TestParseSSE_PingIgnored(t *testing.T) {
	stream := sseStream(
		`event: ping`,
		`data: {"type":"ping"}`,
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":1}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	res, err := parseSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi" {
		t.Errorf("Text = %q, want hi", res.Text)
	}
}

func TestParseSSE_ErrorEventTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	stream := sseStream(`event: error`, `data: {"type":"error","error":{"message":"`+long+`"}}`)
	_, err := parseSSE(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("want error from error event")
	}
	if !strings.Contains(err.Error(), "stream error from provider") {
		t.Errorf("err = %v", err)
	}
	if len(err.Error()) > maxErrorChunkChars+64 {
		t.Errorf("error message not truncated: %d chars", len(err.Error()))
	}
}

func TestParseSSE_ZeroUsageWhenNoMessageDelta(t *testing.T) {
	// Stream ends via message_stop but never saw message_delta (malformed). Usage must be zeroed to avoid
	// exposing {Input:N,Output:0} which triggers the compaction zero-usage local-estimation fallback.
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":42}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	)
	res, err := parseSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 0 || res.Usage.OutputTokens != 0 {
		t.Errorf("Usage = %+v, want zero (no message_delta)", res.Usage)
	}
}

func TestParseSSE_ConnectionDropBeforeMessageStop(t *testing.T) {
	stream := sseStream(
		`event: message_start`,
		`data: {"type":"message_start","message":{"input_tokens":1}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
	)
	_, err := parseSSE(strings.NewReader(stream), nil)
	if !errors.Is(err, errStreamUnterminated) {
		t.Errorf("err = %v, want errStreamUnterminated", err)
	}
}

func TestParseSSE_NoMessageStartIsError(t *testing.T) {
	stream := sseStream(
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
	)
	_, err := parseSSE(strings.NewReader(stream), nil)
	if err == nil || !strings.Contains(err.Error(), "without message_start") {
		t.Errorf("err = %v, want without message_start", err)
	}
}
