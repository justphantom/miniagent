package compaction

import (
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestStripStaleReasoningClearsProviderState(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleAssistant, Reasoning: "old summary", ReasoningState: `[{"type":"reasoning"}]`},
		{Role: miniagent.RoleAssistant, Reasoning: "new summary", ReasoningState: `[{"type":"reasoning","encrypted_content":"opaque"}]`},
	}
	out := stripStaleReasoning(msgs, 1)
	if out[0].Reasoning != "" || out[0].ReasoningState != "" {
		t.Errorf("old reasoning not cleared: %+v", out[0])
	}
	if out[1].Reasoning == "" || out[1].ReasoningState == "" {
		t.Errorf("recent reasoning not retained: %+v", out[1])
	}
	if msgs[0].Reasoning != "old summary" || msgs[0].ReasoningState == "" {
		t.Errorf("caller input mutated: %+v", msgs[0])
	}
}
