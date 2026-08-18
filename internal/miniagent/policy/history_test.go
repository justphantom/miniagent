package policy

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"strings"
	"testing"
)

// trimHistoryForContext: clears reasoning + compresses tool content, without deleting messages or mutating caller input.
func TestTrimHistoryForContext(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a", Reasoning: "long thought", ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "read", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 5000)},
	}
	out := trimHistoryForContext(msgs, 0)

	if out[1].Reasoning != "" {
		t.Errorf("reasoning not cleared: %q", out[1].Reasoning)
	}
	if out[1].ReasoningState != "" {
		t.Errorf("reasoning state not cleared: %q", out[1].ReasoningState)
	}
	if len(out[2].Content) > contextTrimToolChars+50 {
		t.Errorf("tool content not compressed: len=%d (want <= %d+marker)", len(out[2].Content), contextTrimToolChars)
	}
	if len(out) != 3 {
		t.Errorf("messages deleted: got %d, want 3 (pairing must hold)", len(out))
	}
	// caller input is not mutated.
	if msgs[1].Reasoning != "long thought" {
		t.Errorf("caller reasoning mutated")
	}
	if len(msgs[2].Content) != 5000 {
		t.Errorf("caller tool content mutated: len=%d", len(msgs[2].Content))
	}
}
