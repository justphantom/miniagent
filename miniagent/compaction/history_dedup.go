package compaction

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"github.com/justphantom/miniagent/miniagent"
)

// ─── P6 / P8' / P9b: cross-message dedup/folding of tool results and tool_call args ───
//
// All three only modify context-side copies (never touch newMsgs/session), reusing the keepToolArgs
// retention window (the most recent N assistant messages and their tool messages are untouched),
// with the same window semantics as stripStaleToolArgs.
// The trimming trigger is uniformly "a later occurrence of the same key has superseded the earlier one"
// — the earlier one is a pure redundant re-send, compressed to a placeholder with zero/low loss.

// windowStartOf returns the start index of the retention window: i < windowStart is outside
// the window (eligible for dedup/fold), i >= windowStart is inside the window (retained).
// keepN<=0 → len(msgs) (retain no assistant = all outside window = compress all; fixes the original
// return 0 being misinterpreted as "all inside window");
// keepN assistant messages exist → the index of the keepN-th (from the end); fewer than keepN
// assistants → 0 (all inside window = retain all).
func windowStartOf(msgs []miniagent.Message, keepN int) int {
	if keepN <= 0 {
		return len(msgs)
	}
	seen := 0
	for i := range slices.Backward(msgs) {
		if msgs[i].Role == miniagent.RoleAssistant {
			seen++
			if seen == keepN {
				return i
			}
		}
	}
	return 0
}

// argPath extracts the non-empty path field from raw JSON args of a tool call
// (path-type tools like read/write/edit/glob). Returns ("", false) on parse failure or no path.
func argPath(args string) (string, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return "", false
	}
	s, ok := m["path"].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// argReadOffset extracts the offset of read (default 1). P6 groups by (path,offset) —
// the same path with different offsets are different segments of the file and must not overwrite
// each other (v5 §2.1 key constraint).
func argReadOffset(args string) int {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return 1
	}
	if f, ok := m["offset"].(float64); ok && f > 0 {
		return int(f)
	}
	return 1
}

// normalizePath normalizes a tool path for cross-call comparison: filepath.Clean unifies
// "a/./b", "./a", "a//", etc. EvalSymlinks is intentionally skipped — the history phase has no
// workspaceRoot, and paths emitted by the model within the same session share a consistent base,
// so relative-path Clean already covers the vast majority of cases; absolute/relative mixing is
// rare and literal comparison suffices.
func normalizePath(p string) string {
	return filepath.Clean(p)
}

// shellCmdKey extracts a normalized command signature for P9b dedup: splits on whitespace,
// lowercases, rejoins. Treats "ls  -la", "LS -la", "ls -la" as synonymous.
// Returns ("", false) on parse failure or no command.
func shellCmdKey(args string) (string, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return "", false
	}
	s, ok := m["command"].(string)
	if !ok || s == "" {
		return "", false
	}
	fields := strings.Fields(s)
	for i := range fields {
		fields[i] = strings.ToLower(fields[i])
	}
	return strings.Join(fields, " "), true
}

// successWriteEditPaths scans msgs and returns path → ascending list of assistant msgIdx
// for successful write/edit calls. Success = the corresponding tool message has IsError=false
// (failed writes don't modify the file, not counted). Used by P8'/P11 to determine whether a
// later successful write to the same path exists — P8' folds earlier write/edit args based on
// this, P11 folds earlier read results.
func successWriteEditPaths(msgs []miniagent.Message) map[string][]int {
	isErrOf := map[string]bool{}
	for _, m := range msgs {
		if m.Role == miniagent.RoleTool && m.ToolCallID != "" {
			isErrOf[m.ToolCallID] = m.IsError
		}
	}
	succ := map[string][]int{}
	for i, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "write" && tc.Name != "edit" {
				continue
			}
			if isErrOf[tc.ID] { // failed writes don't modify the file, not counted as "successful write"
				continue
			}
			p, ok := argPath(tc.Args)
			if !ok {
				continue
			}
			succ[normalizePath(p)] = append(succ[normalizePath(p)], i)
		}
	}
	return succ
}

// foldStaleWriteEditArgs (P8') folds the args of write/edit tool_calls outside the retention
// window to "path + placeholder" when a later successful write/edit to the same path exists.
// Success is determined via successWriteEditPaths (depends on tool message IsError) — failed
// writes don't modify the file, so the preceding write/edit body is still effective and must
// not be folded (key difference between v5 §2 P8' and P4). Complementary to P4
// (compressToolArgs compresses to a prefix): P4 reduces per-item size, P8' folds the entire
// item when "superseded by a later same-path write". Zero-copy when nothing to fold; deep-copies
// the ToolCalls slice of modified assistant messages.
func foldStaleWriteEditArgs(msgs []miniagent.Message, keepN int) []miniagent.Message {
	succ := successWriteEditPaths(msgs)
	if len(succ) == 0 {
		return msgs
	}
	type we struct {
		msgIdx, tcIdx int
		path          string
	}
	var list []we
	for i, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for j, tc := range m.ToolCalls {
			if tc.Name != "write" && tc.Name != "edit" {
				continue
			}
			p, ok := argPath(tc.Args)
			if !ok {
				continue
			}
			list = append(list, we{i, j, normalizePath(p)})
		}
	}
	if len(list) < 2 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	type foldKey struct{ msgIdx, tcIdx int }
	toFold := map[foldKey]string{}
	for _, e := range list {
		if e.msgIdx >= windowStart {
			continue
		}
		for _, wIdx := range succ[e.path] {
			if wIdx > e.msgIdx { // a later successful write to the same path exists → fold this item's args
				toFold[foldKey{e.msgIdx, e.tcIdx}] = e.path
				break
			}
		}
	}
	if len(toFold) == 0 {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	dirty := map[int]bool{}
	for fk := range toFold {
		dirty[fk.msgIdx] = true
	}
	for i := range out {
		if !dirty[i] {
			continue
		}
		calls := make([]miniagent.ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if p, ok := toFold[foldKey{i, j}]; ok {
				calls[j].Args = foldedWriteEditArgs(p)
			}
		}
		out[i].ToolCalls = calls
	}
	return out
}

// foldedWriteEditArgs constructs the folded args for P8': retains only path + placeholder.
// Old tool_calls in history need not strictly match the tool schema (the endpoint doesn't
// validate historical args); the model only needs to know "this file was modified".
func foldedWriteEditArgs(path string) string {
	b, err := json.Marshal(map[string]any{
		"path":    path,
		"content": "…[prior write/edit content superseded by a later successful write to the same file]",
	})
	if err != nil {
		return `{"path":"` + path + `"}`
	}
	return string(b)
}
