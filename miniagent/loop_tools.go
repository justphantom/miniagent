package miniagent

import (
	"context"
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
