// replay.go implements -replay: it reads the specified session file and offline (no LLM call, no key needed)
// re-translates the persisted message sequence into an NDJSON event stream isomorphic with the runtime,
// faithfully replaying the entire session's ReAct process. Difference from -session: -session loads
// history to continue a new conversation (consumes tokens, produces new events); -replay is purely
// read-only replay, stops when finished.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/event"
	"github.com/justphantom/miniagent/miniagent/session"
)

// runReplay: id → path → load → replay. Failure prints to stderr + exit 1 (error wording matches resolveSessionForRun).
func runReplay(out io.Writer, sessionDir, id string, maxBytes int64) {
	sessPath, err := session.ResolveSessionPath(id, sessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: replay: %v\n", err)
		os.Exit(1)
	}
	meta, msgs, err := session.LoadSession(sessPath, maxBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: load session: %v\n", err)
		os.Exit(1)
	}
	if meta.Type == "" {
		// LoadSession tolerates a half-written tail line; meta.Type=="" means the file does not exist or the first line is missing — error out like the resume path,
		// preventing silent success on a typo.
		fmt.Fprintf(os.Stderr, "miniagent: session %q not found\n", id)
		os.Exit(1)
	}
	if err := replaySession(out, meta, msgs); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: replay: %v\n", err)
		os.Exit(1)
	}
}

// replaySession iterates over msgs and translates them into NDJSON events by role:
//   - assistant: step+1, each tool_call emits a tool_use (and records callID→name for later tool-message lookup);
//     reasoning emits reasoning_delta, content emits text_delta; accumulates that step's usage.
//   - tool: emits tool_result (name resolved via the map; the tool message itself stores only tool_call_id, not name).
//   - user/system: the runtime never emits events for these, so they are skipped.
//
// At the end a result summary is emitted (steps=number of assistant turns, usage accumulated, finish approximated as "stop", model taken from session metadata).
//
// Known precision boundaries (accepted by the user, not defects): text/reasoning are emitted as a whole string at once (the session has no per-chunk splitting);
// tool_result goes through double truncation (the persisted finalized string + EmitToolResult's 2000-char cap) and has no exit_code
// (the session tool message does not store the exit code); a compacted session replays the post-compaction snapshot; finish is always "stop".
func replaySession(w io.Writer, meta session.SessionMeta, msgs []miniagent.Message) error {
	if err := event.EmitSession(w, meta); err != nil {
		return err
	}
	step := 0
	lastText := ""
	callName := map[string]string{}
	var inTok, outTok int
	for _, m := range msgs {
		switch m.Role {
		case miniagent.RoleAssistant:
			step++
			lastText = m.Content
			for _, c := range m.ToolCalls {
				callName[c.ID] = c.Name
				if err := event.EmitToolUse(w, c.Name, c.ID, c.Args, m.Ts); err != nil {
					return err
				}
			}
			if m.Reasoning != "" {
				if err := event.EmitDelta(w, step, miniagent.DeltaReasoning, m.Reasoning, m.Ts); err != nil {
					return err
				}
			}
			if m.Content != "" {
				if err := event.EmitDelta(w, step, miniagent.DeltaText, m.Content, m.Ts); err != nil {
					return err
				}
			}
			if m.Usage != nil {
				inTok += m.Usage.InputTokens
				outTok += m.Usage.OutputTokens
			}
		case miniagent.RoleTool:
			r := miniagent.ToolResult{Output: m.Content, IsError: m.IsError}
			if err := event.EmitToolResult(w, callName[m.ToolCallID], m.ToolCallID, r, m.Ts); err != nil {
				return err
			}
		}
	}
	result := miniagent.Result{
		Text:   lastText,
		Steps:  step,
		Finish: "stop",
		Usage:  miniagent.Usage{InputTokens: inTok, OutputTokens: outTok},
	}
	return event.EmitResult(w, result, meta.Model)
}
