package openai

import (
	"strings"
	"testing"
)

// W1 guard of parseSSE: when a provider omits index and multiple tool_calls collapse onto the same
// index, it errors instead of silently merging (a regression-review blind spot: the guard code was
// correct but lacked a direct test).
func TestParseSSE_ToolCallsMissingIndexErrors(t *testing.T) {
	// call_1 takes index 0; call_2 (different id) also sends index 0 -> same index merges different ids, should error.
	const sse = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"shell","arguments":"{}"}}]}}]}` + "\n" +
		"data: [DONE]\n"
	_, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("two different ids collapsing onto the same index should error (prevents missing index from wrongly merging multiple tool_calls)")
	}
	if !strings.Contains(err.Error(), "index") {
		t.Errorf("error should mention index merge, got %v", err)
	}

	// Contrast: multiple shards with the same id (index 0, first shard carries id + later shards carry only arguments fragments) accumulate normally without error.
	const ok = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}}]}}]}` + "\n" +
		"data: [DONE]\n"
	resp, err := parseSSE(strings.NewReader(ok), nil)
	if err != nil {
		t.Fatalf("multiple shards with the same id should accumulate normally: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" {
		t.Errorf("single tool_call accumulation: got %+v", resp.ToolCalls)
	}
}
