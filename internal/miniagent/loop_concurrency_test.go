package miniagent

import (
	"context"
	"testing"
	"time"
)

// 一步内的多个 tool_call 必须并发执行：3 个工具都启动后才能 release 任一个，
// 串行执行下最多只有 1 个工具会在 release 前启动。
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
	chat, stream := testClients(tr)

	done := make(chan struct{})
	go func() {
		_, _ = Run(context.Background(), chat, stream, LoopConfig{Tools: tools}, "x", LoopHooks{}, nil)
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

// 并行执行下，tool_use 信号仍按 LLM 给定的 tool_call 原序排列。
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
	chat, stream := testClients(tr)
	var uses []string
	onToolUse := func(name, input string) error {
		uses = append(uses, name)
		return nil
	}
	_, err := Run(context.Background(), chat, stream, LoopConfig{Tools: tools}, "x", LoopHooks{OnToolUse: onToolUse}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(uses) != 2 || uses[0] != "b" || uses[1] != "a" {
		t.Errorf("tool_use order = %v, want [b a]", uses)
	}
}

// 已取消的 context 必须立即中止 Run，避免继续烧 token。
func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chat, stream := testClients(&fakeTransport{responses: []string{textResponse("x")}})
	_, err := Run(ctx, chat, stream, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
