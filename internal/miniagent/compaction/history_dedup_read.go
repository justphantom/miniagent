package compaction

import "github.com/justphantom/miniagent/internal/miniagent"

import "strconv"

// dedupReadResults (P6) deduplicates read results outside the retention window by (path,offset):
// each group keeps the last occurrence in time order verbatim; earlier ones are compressed to a
// placeholder. Covers two categories (v5 §2.1): consecutive same (path,offset) reads (P6a, zero loss)
// and read→edit/write→read verification (P6b, removes stale old content). No IsError needed — keeping
// the last occurrence is the correct semantics (the error content of a failed read is naturally
// overridden by a subsequent successful read, which is correct). Different offsets are not merged.
// Zero-copy when nothing to compress.
func dedupReadResults(msgs []miniagent.Message, keepN int) []miniagent.Message {
	readKeyOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "read" {
				continue
			}
			p, ok := argPath(tc.Args)
			if !ok {
				continue
			}
			// key is only for map dedup + placeholder marker text, using a readable separator (path + offset=N);
			// do not use invisible bytes like \x00 — the marker ends up in tool Content → wire request body,
			// which would cause tokenizer/proxy errors or garbled text.
			readKeyOf[tc.ID] = normalizePath(p) + " offset=" + strconv.Itoa(argReadOffset(tc.Args))
		}
	}
	if len(readKeyOf) == 0 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	// last occurrence index of the same key outside the window; whether the same key appears inside
	// the window (if it appears inside the window, all occurrences of that key outside the window are compressed).
	lastIdx := map[string]int{}
	hasInWindow := map[string]bool{}
	for i, m := range msgs {
		if m.Role != miniagent.RoleTool {
			continue
		}
		key, ok := readKeyOf[m.ToolCallID]
		if !ok {
			continue
		}
		if i < windowStart {
			lastIdx[key] = i // ascending traversal overwrites earlier → max index of that key outside window
		} else {
			hasInWindow[key] = true
		}
	}
	toCompress := map[int]string{}
	for i := range windowStart {
		if msgs[i].Role != miniagent.RoleTool {
			continue
		}
		key, ok := readKeyOf[msgs[i].ToolCallID]
		if !ok {
			continue
		}
		if hasInWindow[key] || lastIdx[key] > i {
			toCompress[i] = "…[prior read(" + key + ") superseded by a more recent read]"
		}
	}
	if len(toCompress) == 0 {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	for i, marker := range toCompress {
		out[i].Content = marker
	}
	return out
}

// foldStaleReadResults (P11) folds the Content of read results outside the retention window to a
// placeholder when a later successful write/edit to the same path exists. After a successful
// edit/write the file has changed, and earlier reads return an old version (stale, potentially
// misleading); P6 requires "reading the same (path,offset) again" to clear the preceding read (P6b),
// and P8' only clears write/edit args — both miss the old read that was never followed by another
// read after an edit (v6 §4 measured: this accounts for 36% of payload). P11 complements P8'
// symmetrically: triggered by a later successful write to the same path, keyed by path regardless
// of offset (edit can affect any line, so all offsets of old reads on the same path are stale).
// Success is determined via successWriteEditPaths (IsError). Zero-copy when nothing to compress;
// only modifies the Content copy of tool messages.
func foldStaleReadResults(msgs []miniagent.Message, keepN int) []miniagent.Message {
	readPathOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "read" {
				continue
			}
			if p, ok := argPath(tc.Args); ok {
				readPathOf[tc.ID] = normalizePath(p)
			}
		}
	}
	if len(readPathOf) == 0 {
		return msgs
	}
	succ := successWriteEditPaths(msgs)
	if len(succ) == 0 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	toFold := map[int]string{}
	for i, m := range msgs {
		if m.Role != miniagent.RoleTool || i >= windowStart {
			continue
		}
		rp, ok := readPathOf[m.ToolCallID]
		if !ok {
			continue
		}
		for _, wIdx := range succ[rp] {
			if wIdx > i { // a later successful write to the same path exists → fold the old read result
				toFold[i] = "…[prior read(" + rp + ") result superseded by a later edit]"
				break
			}
		}
	}
	if len(toFold) == 0 {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	for i, marker := range toFold {
		out[i].Content = marker
	}
	return out
}
