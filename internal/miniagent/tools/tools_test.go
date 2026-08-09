package tools

import (
	"context"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"testing"
	"time"
)

// The inner goroutine of runWithTimeout must recover fn's panic, turning it into an IsError result instead of crashing the process.
// Regression: previously go func(){ done <- fn(runCtx) }() had no recover, and safeCall's recover in the caller goroutine
// cannot catch a panic in the inner goroutine, so any panic inside a file tool would punch through the process.
func TestRunWithTimeout_RecoversPanic(t *testing.T) {
	r := runWithTimeout(context.Background(), time.Second, "test", func(_ context.Context) miniagent.ToolResult {
		panic("boom")
	})
	if !r.IsError {
		t.Fatalf("inner-goroutine panic should be recovered into an IsError result: %+v", r)
	}
	if r.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("ExitCode = %d, want miniagent.ExitCodeNotSet", r.ExitCode)
	}
}
