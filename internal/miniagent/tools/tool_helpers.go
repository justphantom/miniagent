// tool_helpers.go: helpers dedicated to tool construction/execution (used only by tool_*.go). Originally in the
// core tools.go, moved here to fix the physical misplacement of "core containing tool-specific helpers", paving
// the way for tool sub-packaging (library-ization 5.0.0). Logically belongs to the tool side, not the core loop;
// co-located in the same package only for historical physical layout, with no logical coupling (uses only public
// types + this group of helpers).

package tools

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"context"
	"path/filepath"
	"time"
)

// resolveToolPath resolves a tool path: returns p unchanged when workspaceRoot is empty or p is already absolute;
// otherwise join(workspaceRoot, p) (join includes Clean, but ../ escaping upwards may resolve outside workdir).
// free mode has **no path boundary constraint**: both ../ and absolute paths can escape workdir; isolation is
// guaranteed by the caller (container/low-privilege user) (README §Execution Isolation). openNoFollow only rejects
// the final symlink component and does not constitute a boundary; the file size cap is unrelated to the boundary.
func resolveToolPath(workspaceRoot, p string) string {
	if workspaceRoot == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workspaceRoot, p)
}

// maxFileResultInHistory is the character cap for results of code-content tools like read/edit entering history:
// code truncation means losing accuracy, so a higher quota than the default policy.MaxToolResultInHistory is given
// (still constrained by read's own maxReadFileChars output cap). miniagent.Tool.ResultLimit takes this value.
const maxFileResultInHistory = 8000

// object builds a JSON Schema object description. When required is empty the key is omitted: the JSON Schema
// spec states that omitting required is equivalent to an empty array, which all compliant backends accept;
// writing a nil slice into the map would serialize as "required":null, triggering a 400 from strict backends
// (e.g. OpenAI).
func object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// runWithTimeout wraps "ctx cancellation check + WithTimeout + goroutine + select fallback" into a single helper,
// reused by file-type tools like read/write/edit/grep/glob/codemap (previously 6 near-verbatim boilerplate copies).
// label goes into the timeout/cancellation message (e.g. "read", "search"). fn receives runCtx (with timeout) and
// can check runCtx during long operations/traversals to return early — but Go cannot forcibly terminate a goroutine:
// single-file syscalls (read/write/edit) stuck in D-state are uninterruptible (OS-level limitation); only the
// WalkDir traversals of grep/glob/codemap can be terminated promptly via runCtx. fn must return promptly, otherwise
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
		return miniagent.ToolResult{IsError: true, Output: label + " timed out or cancelled: " + runCtx.Err().Error()}
	}
}
