package miniagent

import (
	"strings"
	"testing"
)

// trimHistoryForContext：清 reasoning + 压 tool content，不删消息、不改调用方输入。
func TestTrimHistoryForContext(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a", Reasoning: "long thought", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 5000)},
	}
	out := trimHistoryForContext(msgs, 0)

	if out[1].Reasoning != "" {
		t.Errorf("reasoning not cleared: %q", out[1].Reasoning)
	}
	if len(out[2].Content) > contextTrimToolChars+50 {
		t.Errorf("tool content not compressed: len=%d (want <= %d+marker)", len(out[2].Content), contextTrimToolChars)
	}
	if len(out) != 3 {
		t.Errorf("messages deleted: got %d, want 3 (pairing must hold)", len(out))
	}
	// 调用方输入未被修改。
	if msgs[1].Reasoning != "long thought" {
		t.Errorf("caller reasoning mutated")
	}
	if len(msgs[2].Content) != 5000 {
		t.Errorf("caller tool content mutated: len=%d", len(msgs[2].Content))
	}
}
