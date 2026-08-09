package compaction

import (
	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

import (
	"encoding/json"
	"slices"
)

// stripStaleToolArgs (P4) compresses the oversized Args (content/old_string/new_string) of write/edit tool_calls
// belonging to assistant messages older than the most recent keepN, into "prefix + ellipsis marker". Symmetric with
// stripStaleReasoning: it only mutates the context-side copy, never touches newMsgs/session, and leaves ToolCallID
// intact (tool_calls/tool pairing stays complete).
//
// Current blind spot: handleToolCalls pushes tool_call.Args verbatim into history and replays them in full every
// turn, while tool results get trimForHistory's three-tier trimming and tool args get zero trimming — asymmetric.
// write's content (≤10MiB) and edit's old/new_string, once the write succeeds, are already persisted or already
// replaced (old_string no longer exists in the file); the copy in history is pure ballast being resent. Keeping
// path lets the model know which file was changed. Only fields exceeding toolArgsCompressThreshold are compressed
// (small edits untouched); small-args tools like read/grep are not compressed.
// When nothing is compressible (no write/edit session, or all large args are inside the retention window) it
// returns as-is (zero-copy).
func stripStaleToolArgs(msgs []miniagent.Message, keepN int) []miniagent.Message {
	if keepN < 0 {
		keepN = 0
	}
	// Pre-scan: skip the most recent keepN assistants backward, check whether older assistants have compressible tool_calls.
	kept := 0
	hasCompressible := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			if isLargeArgTool(tc.Name) && hasCompressibleArg(tc.Args) {
				hasCompressible = true
				break
			}
		}
		if hasCompressible {
			break
		}
	}
	if !hasCompressible {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	kept = 0
	for i := range slices.Backward(out) {
		if out[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		// Only process when this assistant contains a compressible tool_call. Deep-copy its ToolCalls slice before
		// mutating — copy(msgs) is shallow, the ToolCalls backing array is shared with the caller's input
		// (msgs/History/newMsgs); mutating elements in place would pollute the persistence layer (violating
		// "only mutate the context-side copy").
		compressible := false
		for _, tc := range out[i].ToolCalls {
			if isLargeArgTool(tc.Name) && hasCompressibleArg(tc.Args) {
				compressible = true
				break
			}
		}
		if !compressible {
			continue
		}
		calls := make([]miniagent.ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if isLargeArgTool(calls[j].Name) {
				calls[j].Args = compressToolArgs(calls[j].Args)
			}
		}
		out[i].ToolCalls = calls
	}
	return out
}

// isLargeArgTool reports whether the tool carries potentially oversized write params (write/edit). Other tools have small args and are not compressed.
func isLargeArgTool(name string) bool {
	return name == "write" || name == "edit"
}

// hasCompressibleArg reports whether the args JSON contains a large field exceeding toolArgsCompressThreshold.
func hasCompressibleArg(args string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return false
	}
	if hasLargeStringField(m, "content", "old_string", "new_string") {
		return true
	}
	if edits, ok := m["edits"].([]any); ok {
		for _, e := range edits {
			if em, ok := e.(map[string]any); ok && hasLargeStringField(em, "old_string", "new_string") {
				return true
			}
		}
	}
	return false
}

// hasLargeStringField reports whether any of the specified keys in m has a string value exceeding the compression threshold.
func hasLargeStringField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && len([]rune(s)) > toolArgsCompressThreshold {
			return true
		}
	}
	return false
}

// compressToolArgs compresses oversized fields in the args JSON into "prefix + ellipsis marker". On parse/serialize
// failure, or when no field exceeds the threshold (nothing to compress), it returns the input unchanged — it must
// never break JSON validity (otherwise pairing/wire breaks), and it avoids needless re-serialization (Go emits map
// keys in sorted order, which would reshuffle key order and inject noise). path is always retained so the model
// knows which file was changed.
func compressToolArgs(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return args
	}
	changed := false
	compressField := func(v any) (any, bool) {
		s, ok := v.(string)
		if !ok || len([]rune(s)) <= toolArgsCompressThreshold {
			return v, false
		}
		return text.Truncate(s, toolArgsKeepChars, "…[args omitted]"), true
	}
	for _, k := range []string{"content", "old_string", "new_string"} {
		if _, ok := m[k]; ok {
			nv, c := compressField(m[k])
			m[k] = nv
			if c {
				changed = true
			}
		}
	}
	if edits, ok := m["edits"].([]any); ok {
		for _, e := range edits {
			if em, ok := e.(map[string]any); ok {
				for _, k := range []string{"old_string", "new_string"} {
					if _, ok := em[k]; ok {
						nv, c := compressField(em[k])
						em[k] = nv
						if c {
							changed = true
						}
					}
				}
			}
		}
	}
	if !changed {
		return args
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}
