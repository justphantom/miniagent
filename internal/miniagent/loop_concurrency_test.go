package miniagent

import (
	"context"
	"errors"
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
	llm := testClients(tr)
	var uses []string
	onToolUse := func(name, input string) error {
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

// 已取消的 context 必须立即中止 Run，避免继续烧 token。
// 已取消的 context 必须立即中止 Run，避免继续烧 token。
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
		t.Errorf("calls = %d, want 0（ctx 已取消不应调 LLM）", tr.calls)
	}
}

// T3：一步内多 tool_call 并发执行，其中一个 panic——safeCall 须 recover 回填错误结果，
// 其余工具正常完成，且 assistant.tool_calls 与 tool 消息配对完整（核心不变量）。此前仅测单工具 panic。
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
		t.Fatalf("Run: %v（panic 应被 recover 不应崩进程）", err)
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
		t.Errorf("tool 消息数 = %d, want 3（panic 工具经 safeCall 回填、配对完整）", toolMsgs)
	}
}

// T4：工具执行中 ctx 取消，Run 须及时返回——runToolsParallel 信号量联动 ctx.Done + 工具响应 ctx，
// 否则 wg.Wait 挂死、Run 不响应 SIGINT。此契约（loop_api.go:23）此前零测试。
func TestRun_CtxCancelledDuringToolReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	tool := Tool{
		Name: "block",
		Call: func(c context.Context, _ string) ToolResult {
			close(started)
			<-c.Done()
			return ToolResult{IsError: true, Output: "已取消"}
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
		t.Fatal("工具未启动")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未在 ctx 取消后及时返回（wg.Wait 挂死？）")
	}
}

// M3-4：末轮（step==iterLimit）工具执行期间 ctx 取消——工具返「已取消」（非 error）→ handleToolCalls
// 返 nil → summarizeAtLimit 用已取消 ctx 失败返 ok=false → 循环退出。循环末尾须有 ctx 守卫，否则取消
// 被吞为 FinishMaxIterations + nil（退出码 0 非 130）。MaxIterations=1 使任一工具期取消都命中末轮。
func TestRun_CtxCancelledAtIterationLimitReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	tool := Tool{
		Name: "block",
		Call: func(c context.Context, _ string) ToolResult {
			close(started)
			<-c.Done()
			return ToolResult{IsError: true, Output: "已取消"}
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
		t.Fatal("工具未启动")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled（末轮取消须及时返回，非吞为 max_iterations）", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未在 ctx 取消后及时返回")
	}
}
