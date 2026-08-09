package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

// maxPromptBytes is the size upper bound for a stdin prompt: unbounded reading both bloats memory and,
// after being written back to the session, hits LoadSession's maxSessionBytes limit, making the session
// permanently unresumable. The value is smaller than the session limit.
const maxPromptBytes = 1 << 20

// mustReadPrompt reads the stdin prompt. The read is placed in a goroutine and selected against ctx: io.ReadAll cannot be interrupted by a signal,
// otherwise Ctrl+C cannot break out when interactive without a pipe or with a slow producer (after signal.NotifyContext's channel fills it would also swallow subsequent signals).
// ctx cancellation (SIGINT/SIGTERM) takes code 130 to exit cleanly, consistent with the main Run cancellation path. The read goroutine is reaped by the OS after the process exits.
func mustReadPrompt(ctx context.Context, r io.Reader) []byte {
	type readResult struct {
		prompt []byte
		err    error
	}
	done := make(chan readResult, 1)
	go func() {
		p, err := io.ReadAll(io.LimitReader(r, maxPromptBytes+1))
		done <- readResult{p, err}
	}()
	select {
	case <-ctx.Done():
		os.Exit(130)
	case res := <-done:
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: read stdin: %v\n", res.err)
			os.Exit(1)
		}
		if len(res.prompt) > maxPromptBytes {
			fmt.Fprintf(os.Stderr, "miniagent: stdin prompt exceeds the size limit of %d bytes\n", maxPromptBytes)
			os.Exit(1)
		}
		if len(res.prompt) == 0 {
			fmt.Fprintln(os.Stderr, "miniagent: stdin is empty (send prompt via pipe or redirect)")
			os.Exit(1)
		}
		return res.prompt
	}
	// unreachable: both select branches terminate via os.Exit/return; os.Exit is not recognized by the compiler as terminating, so this is needed to appease it.
	return nil
}
