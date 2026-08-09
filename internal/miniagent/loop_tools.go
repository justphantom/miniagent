package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// maxParallelTools: upper bound of parallel tools per step, to prevent exhausting FDs/connections or triggering target rate limiting.
const maxParallelTools = 8

func safeCall(ctx context.Context, logger *slog.Logger, tool Tool, name, args string) (res ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			if logger != nil {
				logger.Error("tool panic recovered", "tool", name, "panic", r)
			}
			res = ToolResult{IsError: true, ExitCode: ExitCodeNotSet, Output: fmt.Sprintf("tool %q internal error", name)}
		}
	}()
	return tool.Call(ctx, args)
}

func buildToolIndex(tools []Tool, logger *slog.Logger) map[string]Tool {
	toolByName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := toolByName[t.Name]; dup && logger != nil {
			// Silently overwriting duplicates makes the first unreachable with no clue; routing ambiguity is extremely hard to debug.
			logger.Warn("duplicate tool name, last wins", "tool", t.Name)
		}
		toolByName[t.Name] = t
	}
	return toolByName
}

func handleToolCalls(ctx context.Context, cfg LoopConfig, step int, resp Response, toolByName map[string]Tool, msgs []Message, newMsgs *[]Message, hooks LoopHooks, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	// Chain-of-thought enters history with the assistant message (needed to feed back reasoning models). §P0-B: attach real usage for subsequent stale-estimate prevention.
	// Content: resp.Text — models often prepend explanatory text before tool_calls (Claude via OpenAI proxy, some open-source models);
	// including it in history preserves multi-turn coherence; the final text (loop.go:166)/summary (:102) paths both set Content, this aligns (R4-2, was previously discarded).
	appendMsg(&msgs, newMsgs, Message{Role: RoleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, ToolCalls: calls, Usage: &resp.Usage})

	// First notify all tool_use calls in this turn in order: consumers see the complete tool plan as early as possible, with deterministic ordering.
	// OnToolUse returning ErrToolDenied rejects that tool (e.g. dangerous command not confirmed): record then continue
	// notifying the rest; runToolsParallel skips the denied one; other errors still terminate the loop.
	denied := make(map[string]bool)
	if hooks.OnToolUse != nil {
		for _, tc := range calls {
			if err := hooks.OnToolUse(tc.Name, tc.Args); err != nil {
				if errors.Is(err, ErrToolDenied) {
					denied[tc.ID] = true
					continue
				}
				// Pairing backfill: the assistant message (containing all tool_calls) was appended at :52, but runToolsParallel
				// hasn't executed yet — must backfill placeholder tool messages for all calls to preserve pairing (same as
				// OnToolResult(:89)/ShapeToolResult(:103) error paths, where i.. or i+1.. covers the executed range),
				// preventing endpoint 400 on resume.
				fillPlaceholderTail(&msgs, newMsgs, calls, 0)
				return msgs, err
			}
		}
	}

	// Multiple tool_calls from the same LLM turn are mutually independent; serial execution makes total time = Σ individual
	// tool durations (shell can take tens of seconds). Execute in parallel, results backfilled by original index, ensuring
	// history messages correspond one-to-one with assistant.tool_calls (OpenAI requires ordered matching).
	parallel := cfg.MaxParallelTools
	if parallel <= 0 {
		parallel = maxParallelTools
	}
	results := runToolsParallel(ctx, logger, calls, toolByName, denied, parallel)

	for i, tc := range calls {
		tres := results[i]
		if logger != nil {
			logger.Info("tool executed", "step", step, "tool", tc.Name, "is_error", tres.IsError, "output_len", len(tres.Output))
		}
		// Notify consumer of tool results after execution (including ExitCode/is_error), for real-time observation and verification.
		if hooks.OnToolResult != nil {
			if err := hooks.OnToolResult(tc.Name, tc.ID, tres); err != nil {
				// Pairing backfill: downstream unwritable, remaining calls (including current i) results cannot be committed; but assistant.tool_calls are already in history,
				// must backfill a placeholder tool message for each, otherwise Messages pairing breaks and resume gets endpoint 400 (P2-1).
				fillPlaceholderTail(&msgs, newMsgs, calls, i)
				return msgs, err
			}
		}
		// Tool result shaping: via ShapeToolResult hook (truncate/persist/RAG summary etc.). nil = core passes through raw output, no shaping
		// (minimal mode); default implementation NewDefaultShapeToolResult carries the original trimForHistory truncation + optional persist.
		// Only changes tool message content, not role/tool_call_id — pairing invariant guaranteed by core.
		content := tres.Output
		if hooks.ShapeToolResult != nil {
			c, serr := hooks.ShapeToolResult(tc.Name, tc.ID, step, tres)
			if serr != nil {
				// Shaping hook error: current i already executed (OnToolResult already successfully notified with real result), use raw Output
				// into history to stay consistent with what consumer saw; remaining calls backfill placeholders to preserve pairing (unlike OnToolResult error: consumer not confirmed there, i also gets placeholder).
				appendMsg(&msgs, newMsgs, Message{Role: RoleTool, ToolCallID: tc.ID, Content: tres.Output, IsError: tres.IsError})
				// i+1.. tools already executed (runToolsParallel ran all), their real results best-effort notified via OnToolResult — the two hooks' concerns may be independent
				// (e.g. ShapeToolResult persist disk full, while OnToolResult's stdout pipe still usable), otherwise consumer misses results of executed tools.
				// OnToolResult error again: backfill placeholders from that point and return that error (downstream unwritable, further notification is pointless).
				if hooks.OnToolResult != nil {
					for j := i + 1; j < len(calls); j++ {
						if err := hooks.OnToolResult(calls[j].Name, calls[j].ID, results[j]); err != nil {
							// OnToolResult error at j: i+1..j-1 were successfully notified to consumer, but transcript hasn't appended
							// any tool messages (only :106 appended i). So backfill placeholders from i+1 (not j), otherwise i+1..j-1 lack pairing.
							fillPlaceholderTail(&msgs, newMsgs, calls, i+1)
							return msgs, err
						}
					}
				}
				fillPlaceholderTail(&msgs, newMsgs, calls, i+1)
				return msgs, serr
			}
			if c != "" {
				content = c
			}
		}
		appendMsg(&msgs, newMsgs, Message{Role: RoleTool, ToolCallID: tc.ID, Content: content, IsError: tres.IsError})
	}
	return msgs, nil
}

// fillPlaceholderTail backfills one placeholder tool message per call in calls[from:] when downstream is unwritable
// (OnToolResult/ShapeToolResult error), preserving complete Messages pairing — assistant.tool_calls already in history,
// missing pairing causes endpoint 400 on resume.
func fillPlaceholderTail(msgs, newMsgs *[]Message, calls []ToolCall, from int) {
	for j := from; j < len(calls); j++ {
		appendMsg(msgs, newMsgs, Message{Role: RoleTool, ToolCallID: calls[j].ID, Content: "tool result not submitted: upstream pipeline error", IsError: true})
	}
}

// runToolsParallel executes calls in parallel, returning results in the same order as calls.
// Each goroutine writes to a different index in results, no data race; wg.Wait provides happens-before.
// Unknown tools short-circuit before scheduling, directly backfilling error results. Each tool's panic is caught by safeCall.
// Uses a buffered chan as semaphore to limit in-flight tools (default maxParallelTools, cfg.MaxParallelTools can override).
func runToolsParallel(ctx context.Context, logger *slog.Logger, calls []ToolCall, toolByName map[string]Tool, denied map[string]bool, parallel int) []ToolResult {
	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for i, tc := range calls {
		if denied[tc.ID] {
			results[i] = ToolResult{IsError: true, ExitCode: ExitCodeNotSet, Output: "user rejected execution"}
			continue
		}
		tool, ok := toolByName[tc.Name]
		if !ok {
			// ExitCode=ExitCodeNotSet same as denied: unknown tool never actually executed, zero value 0 would be misread by event layer as success (P3-4).
			results[i] = ToolResult{IsError: true, ExitCode: ExitCodeNotSet, Output: fmt.Sprintf("unknown tool %q", tc.Name)}
			continue
		}
		wg.Add(1)
		go func(i int, tc ToolCall, tool Tool) {
			defer wg.Done()
			// Semaphore acquisition tied to ctx: after cancellation, queued calls immediately abort without waiting for a slot;
			// otherwise a ctx-disrespecting blocking tool would hold a slot forever, wg.Wait wouldn't return, Run hangs.
			// Priority-select on ctx.Done: a plain select picks randomly when both a slot and ctx.Done are ready, so a
			// cancelled tool could win the slot and run. Check ctx non-blocking first; only if not yet cancelled block on sem.
			select {
			case <-ctx.Done():
				results[i] = ToolResult{IsError: true, ExitCode: ExitCodeNotSet, Output: "cancelled"}
				return
			default:
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					results[i] = ToolResult{IsError: true, ExitCode: ExitCodeNotSet, Output: "cancelled"}
					return
				}
			}
			defer func() { <-sem }()
			results[i] = safeCall(ctx, logger, tool, tc.Name, tc.Args)
		}(i, tc, tool)
	}
	wg.Wait()
	return results
}
