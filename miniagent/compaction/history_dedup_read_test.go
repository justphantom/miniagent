package compaction

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

func TestDedupReadResults(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: "user", Content: "q"}, // 0
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}}, // 1
		{Role: "tool", ToolCallID: "r1", Content: "first read"},                                                 // 2
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r2", Name: "read", Args: `{"path":"a.go"}`}}}, // 3
		{Role: "tool", ToolCallID: "r2", Content: "second read"},                                                // 4
	}
	out := dedupReadResults(msgs, 1) // retain the most recent 1 assistant (idx3)
	if out[2].Content == "first read" {
		t.Errorf("old read result should be folded to a placeholder, got %q", out[2].Content)
	}
	if !strings.Contains(out[2].Content, "superseded by a more recent read") || !strings.Contains(out[2].Content, "a.go") {
		t.Errorf("old read placeholder should contain the path and superseded note, got %q", out[2].Content)
	}
	if out[4].Content != "second read" {
		t.Errorf("the newest read inside the window should keep its original text, got %q", out[4].Content)
	}
	if msgs[2].Content != "first read" {
		t.Errorf("caller input was modified")
	}

	// different offsets are not merged.
	diff := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go","offset":1}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "seg1"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r2", Name: "read", Args: `{"path":"a.go","offset":100}`}}},
		{Role: "tool", ToolCallID: "r2", Content: "seg100"},
	}
	if got := dedupReadResults(diff, 1); got[1].Content != "seg1" {
		t.Errorf("same path reads with different offsets should not be merged, got %q", got[1].Content)
	}

	// no read → zero-copy.
	plain := []miniagent.Message{{Role: "user", Content: "q"}}
	if got := dedupReadResults(plain, 1); &got[0] != &plain[0] {
		t.Errorf("session without read should return the same slice as-is")
	}
	// all inside the retention window → zero-copy.
	if got := dedupReadResults(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("all inside window should return as-is")
	}
}

func TestFoldStaleReadResults(t *testing.T) {
	// read(a) → edit(a) succeeds: old read result is folded.
	msgs := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "old content of a.go"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "e1", Name: "edit", Args: `{"path":"a.go","old_string":"x","new_string":"y"}`}}},
		{Role: "tool", ToolCallID: "e1", Content: "ok", IsError: false},
	}
	out := foldStaleReadResults(msgs, 1) // retain the most recent 1 assistant (idx2)
	if out[1].Content == "old content of a.go" {
		t.Errorf("old read result should be folded after a successful edit, got %q", out[1].Content)
	}
	if !strings.Contains(out[1].Content, "superseded by a later edit") || !strings.Contains(out[1].Content, "a.go") {
		t.Errorf("folded placeholder should contain path and superseded note, got %q", out[1].Content)
	}
	if msgs[1].Content != "old content of a.go" {
		t.Errorf("caller input was modified")
	}

	// edit fails: not folded (file unchanged, old read still effective).
	failEdit := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "content"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "e1", Name: "edit", Args: `{"path":"a.go","old_string":"x","new_string":"y"}`}}},
		{Role: "tool", ToolCallID: "e1", Content: "err", IsError: true},
	}
	if got := foldStaleReadResults(failEdit, 1); got[1].Content != "content" {
		t.Errorf("old read should not be folded when edit fails, got %q", got[1].Content)
	}

	// different path: not folded.
	diffPath := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "a"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "e1", Name: "edit", Args: `{"path":"b.go","old_string":"x","new_string":"y"}`}}},
		{Role: "tool", ToolCallID: "e1", Content: "ok"},
	}
	if got := foldStaleReadResults(diffPath, 1); got[1].Content != "a" {
		t.Errorf("edit on a different path should not trigger read folding, got %q", got[1].Content)
	}

	// successful write triggers folding as well (whole-file overwrite, old read must be stale).
	writeMsgs := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "old"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"new"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
	}
	if out2 := foldStaleReadResults(writeMsgs, 1); !strings.Contains(out2[1].Content, "superseded by a later edit") {
		t.Errorf("old read should also be folded after a successful write, got %q", out2[1].Content)
	}

	// read inside the retention window is untouched (keepN covers the assistant where the read resides).
	if got := foldStaleReadResults(msgs, 2); got[1].Content != "old content of a.go" {
		t.Errorf("read inside the retention window should not be folded, got %q", got[1].Content)
	}

	// no read → zero-copy.
	noRead := []miniagent.Message{{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"x"}`}}}}
	if got := foldStaleReadResults(noRead, 1); &got[0] != &noRead[0] {
		t.Errorf("session without read should return as-is")
	}
	// no write/edit → zero-copy.
	noWrite := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "x"},
	}
	if got := foldStaleReadResults(noWrite, 1); &got[0] != &noWrite[0] {
		t.Errorf("session without write/edit should return as-is")
	}
}
