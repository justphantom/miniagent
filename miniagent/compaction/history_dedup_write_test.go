package compaction

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

func TestFoldStaleWriteEditArgs(t *testing.T) {
	// two successful writes to the same path: the earlier one is folded.
	msgs := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"old"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w2", Name: "write", Args: `{"path":"a.go","content":"new"}`}}},
		{Role: "tool", ToolCallID: "w2", Content: "ok"},
	}
	out := foldStaleWriteEditArgs(msgs, 1) // retain the most recent 1 assistant (idx2)
	if !strings.Contains(out[0].ToolCalls[0].Args, "superseded by a later successful write to the same file") {
		t.Errorf("old write args superseded by a later successful write should be folded, got %q", out[0].ToolCalls[0].Args)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0].ToolCalls[0].Args), &m); err != nil {
		t.Errorf("folded args not valid JSON: %q (err %v)", out[0].ToolCalls[0].Args, err)
	}
	if m["path"] != "a.go" {
		t.Errorf("folded args should retain path, got %v", m["path"])
	}
	if out[2].ToolCalls[0].Args != msgs[2].ToolCalls[0].Args {
		t.Errorf("the newest write args inside the window should keep its original text")
	}
	if !strings.Contains(msgs[0].ToolCalls[0].Args, "old") {
		t.Errorf("caller input was modified")
	}

	// a later failed write does not trigger folding (the preceding content is still effective).
	failLater := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"v1"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok", IsError: false},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w2", Name: "write", Args: `{"path":"a.go","content":"v2"}`}}},
		{Role: "tool", ToolCallID: "w2", Content: "err", IsError: true},
	}
	if got := foldStaleWriteEditArgs(failLater, 1); got[0].ToolCalls[0].Args != failLater[0].ToolCalls[0].Args {
		t.Errorf("a later failed write should not trigger folding earlier args")
	}

	// different paths are not folded against each other.
	diffPath := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"x"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w2", Name: "write", Args: `{"path":"b.go","content":"y"}`}}},
		{Role: "tool", ToolCallID: "w2", Content: "ok"},
	}
	if got := foldStaleWriteEditArgs(diffPath, 1); got[0].ToolCalls[0].Args != diffPath[0].ToolCalls[0].Args {
		t.Errorf("writes to different paths should not be folded against each other")
	}

	// single write → zero-copy.
	single := []miniagent.Message{{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"x"}`}}}}
	if got := foldStaleWriteEditArgs(single, 0); &got[0] != &single[0] {
		t.Errorf("single-write session should return as-is")
	}
	// all inside the retention window → zero-copy.
	if got := foldStaleWriteEditArgs(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("all inside window should return as-is")
	}
}
