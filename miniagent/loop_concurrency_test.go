package miniagent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Multiple tool_calls within a single step must execute concurrently: all 3 tools must have started before
// any one can be released; under serial execution at most 1 tool would start before release.
func TestRun_ToolsRunInParallel(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	mk := func(name string) Tool {
		return Tool{
			Name: name,
			Call: func(context.Context, string) ToolResult {
				started <- name
				<-release
				return ToolResult{Output: name}
			},
		}
	}
	tools := []Tool{mk("a"), mk("b"), mk("c")}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "1", Name: "a", Args: "{}"},
			ToolCall{ID: "2", Name: "b", Args: "{}"},
			ToolCall{ID: "3", Name: "c", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)

	done := make(chan struct{})
	go func() {
		_, _ = Run(context.Background(), llm, LoopConfig{Tools: tools}, "x", LoopHooks{}, nil)
		close(done)
	}()

	got := make(map[string]bool, 3)
	for range 3 {
		select {
		case name := <-started:
			got[name] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d tools started before release, expected 3 (got %v)", len(got), got)
		}
	}
	close(release)
	<-done
}

// Under parallel execution, tool_use signals are still ordered as the LLM gave the tool_calls.
func TestRun_ParallelToolResultsMatchOrder(t *testing.T) {
	tools := []Tool{
		{Name: "a", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "A"} }},
		{Name: "b", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "B"} }},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "1", Name: "b", Args: "{}"},
			ToolCall{ID: "2", Name: "a", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	var uses []string
	onToolUse := func(name, callID, input string) error {
		uses = append(uses, name)
		return nil
	}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: tools}, "x", LoopHooks{OnToolUse: onToolUse}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(uses) != 2 || uses[0] != "b" || uses[1] != "a" {
		t.Errorf("tool_use order = %v, want [b a]", uses)
	}
}

// An already-cancelled context must abort Run immediately to avoid burning more tokens.
// An already-cancelled context must abort Run immediately to avoid burning more tokens.
func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := testClients(tr)
	_, err := Run(ctx, llm, LoopConfig{}, "hi", LoopHooks{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if tr.calls != 0 {
		t.Errorf("calls = %d, want 0 (ctx already cancelled, should not call the LLM)", tr.calls)
	}
}

// T3: multiple tool_calls execute concurrently within a single step and one of them panics — safeCall must
// recover and backfill an error result, the other tools complete normally, and assistant.tool_calls stays
// fully paired with tool messages (a core invariant). Previously only single-tool panic was tested.
func TestRun_ConcurrentToolPanicRecovers(t *testing.T) {
	tools := []Tool{
		{Name: "a", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "A"} }},
		{Name: "boom", Call: func(context.Context, string) ToolResult { panic("boom") }},
		{Name: "c", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "C"} }},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "1", Name: "a", Args: "{}"},
			ToolCall{ID: "2", Name: "boom", Args: "{}"},
			ToolCall{ID: "3", Name: "c", Args: "{}"},
		),
		textResponse("done"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: tools}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v (panic should be recovered, must not crash the process)", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	var toolMsgs int
	for _, m := range res.Messages {
		if m.Role == RoleTool {
			toolMsgs++
		}
	}
	if toolMsgs != 3 {
		t.Errorf("tool message count = %d, want 3 (the panicking tool is backfilled via safeCall; pairing is complete)", toolMsgs)
	}
}

// T4: when the ctx is cancelled during tool execution, Run must return promptly — the runToolsParallel
// semaphore ties ctx.Done together with the tool's response ctx; otherwise wg.Wait hangs and Run would not
// respond to SIGINT. This contract (loop_api.go:23) previously had zero tests.
func TestRun_CtxCancelledDuringToolReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	tool := Tool{
		Name: "block",
		Call: func(c context.Context, _ string) ToolResult {
			close(started)
			<-c.Done()
			return ToolResult{IsError: true, Output: "cancelled"}
		},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "1", Name: "block", Args: "{}"}),
	}}
	llm := testClients(tr)
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation (wg.Wait hung?)")
	}
}

// M3-4: ctx is cancelled during tool execution in the final step (step==iterLimit) — the tool returns
// "cancelled" (not an error) → handleToolCalls returns nil → summarizeAtLimit fails with ok=false on the
// already-cancelled ctx → the loop exits. A ctx guard is needed at the end of the loop; otherwise the
// cancellation is swallowed as FinishMaxIterations + nil (exit code 0 instead of 130). MaxIterations=1 makes
// any tool-time cancellation hit the final step.
func TestRun_CtxCancelledAtIterationLimitReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	tool := Tool{
		Name: "block",
		Call: func(c context.Context, _ string) ToolResult {
			close(started)
			<-c.Done()
			return ToolResult{IsError: true, Output: "cancelled"}
		},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "1", Name: "block", Args: "{}"}),
	}}
	llm := testClients(tr)
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}, "x", LoopHooks{}, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled (cancellation in the final step must return promptly, not be swallowed as max_iterations)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation")
	}
}
