package tools

import (
	"context"
	"fmt"
	"time"

	miniagent "github.com/justphantom/miniagent/miniagent"
)

// runWithTimeout wraps "ctx cancellation check + WithTimeout + goroutine + select fallback" into a single helper,
// reused by file-type tools like read/write/edit/grep/glob (previously 5 near-verbatim boilerplate copies).
// label goes into the timeout/cancellation message (e.g. "read", "search"). fn receives runCtx (with timeout) and
// can check runCtx during long operations/traversals to return early — but Go cannot forcibly terminate a goroutine:
// single-file syscalls (read/write/edit) stuck in D-state are uninterruptible (OS-level limitation); only the
// WalkDir traversals of grep/glob can be terminated promptly via runCtx. fn must return promptly, otherwise
// after the select fallback the goroutine still runs until fn ends naturally (done buffered=1 guarantees the send
// won't block, but does not guarantee fn is interruptible).
func runWithTimeout(ctx context.Context, timeout time.Duration, label string, fn func(ctx context.Context) miniagent.ToolResult) miniagent.ToolResult {
	if err := ctx.Err(); err != nil {
		return miniagent.ToolResult{IsError: true, Output: "cancelled: " + err.Error()}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan miniagent.ToolResult, 1)
	// self-recover inside the goroutine: fn runs in this goroutine, so the caller's safeCall recover cannot catch it —
	// symmetric with safeCall (loop_tools.go)/callLLMOnce; a panic inside file tools is converted to an IsError result
	// instead of crashing the process.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: label + " internal error"}
			}
		}()
		done <- fn(runCtx)
	}()
	select {
	case r := <-done:
		return r
	case <-runCtx.Done():
		// Timeout message carries the duration (the LLM cannot infer it from ctx error strings), and distinguishes
		// the two causes: a parent cancellation surfaces ctx.Err(), a tool's own timeout names the duration — the LLM
		// can then decide "split the command / narrow the test set" instead of mistaking it for a command failure.
		if ctx.Err() != nil {
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: label + " cancelled: " + ctx.Err().Error()}
		}
		// Grace window: fn's kill path (runLimitedOutput closes the pipe on ctx.Done) unblocks the read
		// loop almost immediately — the partial output captured so far (last test names, progress) is the
		// only clue to WHERE it hung, so prefer a late result over an instant bare timeout line.
		// A late fn result passes through UNMODIFIED: for file tools the write may have committed by
		// then (re-labeling a finished edit as "timed out" makes the LLM take recovery action on a
		// false premise), and an exec tool's non-zero exit is a normal result (exitAwareResult contract)
		// whose ExitCode must not be discarded.
		select {
		case r := <-done:
			return r
		case <-time.After(2 * time.Second):
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: fmt.Sprintf("%s timed out after %s — narrow the scope (fewer packages / smaller command) and retry", label, timeout)}
		}
	}
}
