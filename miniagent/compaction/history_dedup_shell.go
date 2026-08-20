package compaction

import (
	"encoding/json"

	"github.com/justphantom/miniagent/miniagent"
)

// dedupShellCommands (P9b) deduplicates shell tool_calls outside the retention window by
// normalized command signature: each group keeps the last occurrence verbatim in time order;
// earlier synonymous commands are folded to a placeholder, AND the corresponding role=tool
// RESULT Content is folded symmetrically (keep-last verbatim, placeholder earlier ones — mirroring
// P6 in history_dedup_read.go; shell output for the same command often differs between runs, so
// only earlier results are safe to drop). ReAct sessions frequently repeat exploration commands
// (pwd && ls, find ... -name); earlier synonymous commands have no value for subsequent decisions
// (v5 §3 P9b). No IsError needed. Deep-copies the ToolCalls slice of modified assistant messages.
func dedupShellCommands(msgs []miniagent.Message, keepN int) []miniagent.Message {
	shellKeyOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "shell" {
				continue
			}
			if k, ok := shellCmdKey(tc.Args); ok {
				shellKeyOf[tc.ID] = k
			}
		}
	}
	if len(shellKeyOf) == 0 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	type se struct {
		msgIdx, tcIdx, order int
		key                  string
	}
	var list []se
	order := 0
	for i, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for j, tc := range m.ToolCalls {
			k, ok := shellKeyOf[tc.ID]
			if !ok {
				continue
			}
			list = append(list, se{i, j, order, k})
			order++
		}
	}
	hasInWindow := map[string]bool{}
	lastOrder := map[string]int{}
	for _, e := range list {
		if e.msgIdx >= windowStart {
			hasInWindow[e.key] = true
		} else {
			lastOrder[e.key] = e.order
		}
	}
	type foldKey struct{ msgIdx, tcIdx int }
	toFold := map[foldKey]bool{}
	for _, e := range list {
		if e.msgIdx >= windowStart {
			continue
		}
		if hasInWindow[e.key] || lastOrder[e.key] > e.order {
			toFold[foldKey{e.msgIdx, e.tcIdx}] = true
		}
	}
	if len(toFold) == 0 {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	// foldedIDs collects the tool_call IDs being folded (earlier occurrences; the last stays verbatim).
	// It drives a second pass that ALSO folds the corresponding role=tool RESULT Content — symmetric
	// with the args folding and mirroring P6/P11 in history_dedup_read.go (keep-last-fold-earlier).
	// Shell output for the same command frequently differs between runs (re-running go test / git
	// status is the point of re-running), so only the already-folded earlier results are placeholdered;
	// the last occurrence's ID is not in toFold, so its result stays verbatim.
	foldedIDs := map[string]bool{}
	dirty := map[int]bool{}
	for fk := range toFold {
		dirty[fk.msgIdx] = true
		foldedIDs[out[fk.msgIdx].ToolCalls[fk.tcIdx].ID] = true
	}
	for i := range out {
		if !dirty[i] {
			continue
		}
		calls := make([]miniagent.ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if toFold[foldKey{i, j}] {
				// json.Marshal constructs the placeholder (consistent with foldedWriteEditArgs), preventing
				// hardcoded JSON from breaking due to quotes/backslashes introduced by the text.
				placeholder, _ := json.Marshal(map[string]string{"command": "…[prior identical shell command superseded by a later execution]"})
				calls[j].Args = string(placeholder)
			}
		}
		out[i].ToolCalls = calls
	}
	// Fold the tool RESULT Content for the same ToolCallIDs. Content is a value field, so assigning on
	// the slice-element copy leaves the caller's input untouched — the same pattern P6/P11 rely on.
	for i := range out {
		if out[i].Role != miniagent.RoleTool || !foldedIDs[out[i].ToolCallID] {
			continue
		}
		out[i].Content = "…[prior identical shell command result superseded by a later execution]"
	}
	return out
}
