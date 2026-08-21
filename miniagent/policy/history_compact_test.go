package policy

import (
	miniagent "github.com/justphantom/miniagent/miniagent"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	// Pure ASCII: 4 chars ≈ 1 token; with an empty system + no tools it equals content + systemOverhead + per-message envelope.
	if n := miniagent.EstimateTokens([]miniagent.Message{{Role: "user", Content: "abcdefgh"}}, "", nil); n != 2+miniagent.SystemOverheadTokens+miniagent.EnvelopePerMsgTokens {
		t.Errorf("ascii 8 chars = %d, want %d", n, 2+miniagent.SystemOverheadTokens+miniagent.EnvelopePerMsgTokens)
	}
	// Pure CJK: 2 chars ≈ 1 token
	if n := miniagent.EstimateTokens([]miniagent.Message{{Role: "user", Content: "四个汉字"}}, "", nil); n != 2+miniagent.SystemOverheadTokens+miniagent.EnvelopePerMsgTokens {
		t.Errorf("cjk 4 chars = %d, want %d", n, 2+miniagent.SystemOverheadTokens+miniagent.EnvelopePerMsgTokens)
	}
	// tool_calls.Args is counted in the estimate; each tool_call adds an extra envelope (the nested function object).
	if n := miniagent.EstimateTokens([]miniagent.Message{{Role: "assistant", ToolCalls: []miniagent.ToolCall{{Args: "abcd"}}}}, "", nil); n != 1+miniagent.SystemOverheadTokens+miniagent.EnvelopePerMsgTokens+miniagent.EnvelopePerToolCallTokens {
		t.Errorf("args 4 chars = %d, want %d", n, 1+miniagent.SystemOverheadTokens+miniagent.EnvelopePerMsgTokens+miniagent.EnvelopePerToolCallTokens)
	}
}

// estimateTokens blind spot: the fixed overhead of system prompt content + tool schema must be counted,
// otherwise compaction triggers too late. Uses delta assertions so the test does not depend on concrete constant values.
func TestEstimateTokens_Overhead(t *testing.T) {
	msgs := []miniagent.Message{{Role: "user", Content: "abcdefgh"}}
	base := miniagent.EstimateTokens(msgs, "", nil)
	if got := miniagent.EstimateTokens(msgs, "abcd", nil) - base; got != 1 {
		t.Errorf("system 4 chars should add 1 token, got delta %d", got)
	}
	small := miniagent.EstimateTokens(msgs, "", []miniagent.Tool{{Name: "t", Description: "abcd"}}) - base
	large := miniagent.EstimateTokens(msgs, "", []miniagent.Tool{{Name: "t", Description: strings.Repeat("a", 400)}}) - base
	if large <= small {
		t.Errorf("schema estimate should grow with description length: small=%d large=%d", small, large)
	}
	if small <= 0 {
		t.Errorf("non-empty tool schema should add tokens: small=%d", small)
	}
	if got := miniagent.EstimateTokens(msgs, strings.Repeat("a", 40), nil) - base; got != 10 {
		t.Errorf("system 40 chars should add 10 tokens, got delta %d", got)
	}
}
