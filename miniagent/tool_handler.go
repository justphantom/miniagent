package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// handleToolCalls processes and executes the tool calls from the LLM response.
// It appends the assistant message with tool_calls to history, notifies OnToolUse hooks,
// runs tools in parallel via runToolsParallel, then processes results through OnToolResult
// and ShapeToolResult hooks. On error, it backfills placeholder tool messages to preserve pairing.
func handleToolCalls(ctx context.Context, cfg LoopConfig, step int, resp Response, toolByName map[string]Tool, msgs []Message, newMsgs *[]Message, hooks LoopHooks, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	// Chain-of-thought enters history with the assistant message (needed to feed back reasoning models).
	appendMsg(&msgs, newMsgs, Message{Role: RoleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, ReasoningState: resp.ReasoningState, ToolCalls: calls, Usage: &resp.Usage})

	// First notify all tool_use calls in this turn in order: consumers see the complete tool plan as early as possible.
	denied := make(map[string]bool)
	if hooks.OnToolUse != nil {
		for _, tc := range calls {
			if err := hooks.OnToolUse(tc.Name, tc.Args); err != nil {
				if errors.Is(err, ErrToolDenied) {
					denied[tc.ID] = true
					continue
				}
				fillPlaceholderTail(&msgs, newMsgs, calls, 0)
				return msgs, err
			}
		}
	}

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
		if hooks.OnToolResult != nil {
			if err := hooks.OnToolResult(tc.Name, tc.ID, tres); err != nil {
				fillPlaceholderTail(&msgs, newMsgs, calls, i)
				return msgs, err
			}
		}
		content := tres.Output
		if hooks.ShapeToolResult != nil {
			c, serr := hooks.ShapeToolResult(tc.Name, tc.ID, step, tres)
			if serr != nil {
				appendMsg(&msgs, newMsgs, Message{Role: RoleTool, ToolCallID: tc.ID, Content: tres.Output, IsError: tres.IsError})
				if hooks.OnToolResult != nil {
					for j := i + 1; j < len(calls); j++ {
						if err := hooks.OnToolResult(calls[j].Name, calls[j].ID, results[j]); err != nil {
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
// (OnToolResult/ShapeToolResult error), preserving complete Messages pairing.
func fillPlaceholderTail(msgs, newMsgs *[]Message, calls []ToolCall, from int) {
	for j := from; j < len(calls); j++ {
		appendMsg(msgs, newMsgs, Message{Role: RoleTool, ToolCallID: calls[j].ID, Content: "tool result not submitted: upstream pipeline error", IsError: true})
	}
}
