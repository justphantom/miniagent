package compaction

import "github.com/justphantom/miniagent/internal/miniagent"

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// dedupReadResults (P6): same (path,offset) keeps the last occurrence, earlier ones are folded to a placeholder;
// different offsets are not merged; messages inside the retention window are untouched; no read → zero-copy;
// does not modify the caller's input.
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

// foldStaleWriteEditArgs (P8'): a later successful write/edit to the same path triggers folding
// of earlier args; a later failure does not trigger; different paths are not folded against each other;
// single item / full window → zero-copy; does not modify the caller's input.
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

// dedupShellCommands (P9b): deduplicates synonymous commands by normalization, keeping the last occurrence;
// different commands are not merged; no shell → zero-copy; full window → zero-copy; does not modify the caller's input.
func TestDedupShellCommands(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"ls -la"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: "out1"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"LS  -la"}`}}}, // normalized synonym
		{Role: "tool", ToolCallID: "s2", Content: "out2"},
	}
	out := dedupShellCommands(msgs, 1)
	if !strings.Contains(out[0].ToolCalls[0].Args, "superseded by a later execution") {
		t.Errorf("earlier synonymous shell command should be folded, got %q", out[0].ToolCalls[0].Args)
	}
	if out[2].ToolCalls[0].Args != msgs[2].ToolCalls[0].Args {
		t.Errorf("the newest shell command inside the window should keep its original text")
	}
	if !strings.Contains(msgs[0].ToolCalls[0].Args, "ls -la") {
		t.Errorf("caller input was modified")
	}

	// different commands are not merged.
	diff := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"pwd"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: "/"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"ls"}`}}},
		{Role: "tool", ToolCallID: "s2", Content: "f"},
	}
	if got := dedupShellCommands(diff, 1); got[0].ToolCalls[0].Args != diff[0].ToolCalls[0].Args {
		t.Errorf("different shell commands should not be merged")
	}

	// no shell → zero-copy.
	plain := []miniagent.Message{{Role: "user", Content: "q"}}
	if got := dedupShellCommands(plain, 1); &got[0] != &plain[0] {
		t.Errorf("session without shell should return as-is")
	}
	// all inside the retention window → zero-copy.
	if got := dedupShellCommands(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("all inside window should return as-is")
	}
}

// foldStaleReadResults (P11): a later successful write/edit to the same path triggers folding of earlier read results;
// a later failure does not trigger; different paths do not trigger; write triggers it as well; messages inside
// the retention window are untouched; no read / no write → zero-copy; does not modify the caller's input.
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

// applyContextStrips: at Debug level logs the tokens saved by each strip stage (P11 triggers);
// at Info level zero output.
func TestApplyContextStrips_Debug(t *testing.T) {
	big := strings.Repeat("x", 4000)
	msgs := []miniagent.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: big},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"` + big + `"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
		{Role: "assistant", Content: "done1"},
		{Role: "assistant", Content: "done2"},
	}
	// Debug level: P11 should fold read r1 (superseded by write w1, outside the window); the log contains stage + saved_tokens + fit done.
	var buf bytes.Buffer
	dbg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	applyContextStrips(context.Background(), msgs, 1, 0, 2, dbg, "", nil)
	logs := buf.String()
	if !strings.Contains(logs, "P11_foldRead") || !strings.Contains(logs, "saved_tokens") {
		t.Errorf("Debug log should contain P11_foldRead and saved_tokens, got:\n%s", logs)
	}
	if !strings.Contains(logs, "fit done") {
		t.Errorf("Debug log should contain the fit done summary")
	}
	// Info level: no strip saved log (zero overhead).
	var buf2 bytes.Buffer
	info := slog.New(slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo}))
	applyContextStrips(context.Background(), msgs, 1, 0, 2, info, "", nil)
	if strings.Contains(buf2.String(), "strip saved") {
		t.Errorf("Info level should not output strip saved")
	}
}
