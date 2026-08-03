package miniagent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// OnToolResult 在每个工具执行后触发一次，透传 name/call_id 与结果（含 IsError）。
func TestRun_OnToolResultFired(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "res"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	var got []string
	hooks := LoopHooks{
		OnToolResult: func(name, callID string, r ToolResult) error {
			got = append(got, name+":"+callID+":"+r.Output)
			return nil
		},
	}
	if _, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0] != "q:c1:res" {
		t.Errorf("OnToolResult calls = %v, want [q:c1:res]", got)
	}
}

// OnToolResult 返回 error 沿链上抛终止循环（与 OnToolUse 同语义：下游不可写时停）。
func TestRun_OnToolResultErrorStops(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	stop := errors.New("downstream closed")
	hooks := LoopHooks{OnToolResult: func(string, string, ToolResult) error { return stop }}
	_, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
}

// OnToolUse 返回 ErrToolDenied：该工具被拒（回填拒绝结果）、不执行；其他工具正常。
func TestRun_ToolDeniedSkipped(t *testing.T) {
	toolA := Tool{Name: "a", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "A_ran"} }}
	toolB := Tool{Name: "b", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "B_ran"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "ca", Name: "a", Args: "{}"},
			ToolCall{ID: "cb", Name: "b", Args: "{}"},
		),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	hooks := LoopHooks{OnToolUse: func(name, input string) error {
		if name == "a" {
			return ErrToolDenied
		}
		return nil
	}}
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{toolA, toolB}}, "x", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var aOut, bOut string
	for _, m := range res.Messages {
		if m.Role != roleTool {
			continue
		}
		if m.ToolCallID == "ca" {
			aOut = m.Content
		}
		if m.ToolCallID == "cb" {
			bOut = m.Content
		}
	}
	if !strings.Contains(aOut, "拒绝") {
		t.Errorf("denied tool a should be rejected: %q", aOut)
	}
	if !strings.Contains(bOut, "B_ran") {
		t.Errorf("tool b should still run: %q", bOut)
	}
}

// P1-5：usage 全零（端点不返回 usage）时 Run 用本地估算 fallback 并继续运行，同时 warn 暴露。
func TestRun_ZeroUsageWarns(t *testing.T) {
	// 响应不含 usage 字段 → parseChatResponse 得零值 Usage（流式端点常见的现实情形）。
	noUsage := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	tr := &fakeTransport{responses: []string{noUsage}}
	chat, stream := testClients(tr)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	res, err := Run(context.Background(), chat, stream, LoopConfig{Model: "m", MaxTotalTokens: 10000}, "q", LoopHooks{}, logger)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("Text = %q, want hi", res.Text)
	}
	if !strings.Contains(buf.String(), "llm returned no usage") {
		t.Errorf("expected warn about missing usage, got logs: %s", buf.String())
	}
}

// P1-5-b：usage 全零时本地估算仍触发 MaxTotalTokens 预算熔断。
func TestRun_ZeroUsageBudgetEnforced(t *testing.T) {
	noUsage := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	tr := &fakeTransport{responses: []string{noUsage}}
	chat, stream := testClients(tr)
	_, err := Run(context.Background(), chat, stream, LoopConfig{Model: "m", MaxTotalTokens: 100}, "q", LoopHooks{}, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
}

// P2-1：OnToolResult 中途 error 时，assistant.tool_calls 的每个 id 都应有对应 tool 消息，
// 保证 Messages 配对完整可续跑（端点不会因孤立 tool_call 返回 400）。
func TestRun_OnToolResultErrorKeepsPairing(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(
			ToolCall{ID: "c0", Name: "q", Args: "{}"},
			ToolCall{ID: "c1", Name: "q", Args: "{}"},
			ToolCall{ID: "c2", Name: "q", Args: "{}"},
		),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	stop := errors.New("downstream closed")
	hooks := LoopHooks{OnToolResult: func(name, callID string, r ToolResult) error {
		if callID == "c1" {
			return stop // 在第 2 个工具处中断下游
		}
		return nil
	}}
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
	// 校验每个 assistant.tool_call 的 id 都有匹配的 tool 消息。
	var wantIDs []string
	gotIDs := make(map[string]bool)
	for _, m := range res.Messages {
		if m.Role == roleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				wantIDs = append(wantIDs, tc.ID)
			}
		}
		if m.Role == roleTool {
			gotIDs[m.ToolCallID] = true
		}
	}
	if len(wantIDs) == 0 {
		t.Fatalf("no assistant tool_calls found; msgs=%+v", res.Messages)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("tool_call %q has no matching tool message (pairing broken); msgs=%+v", id, res.Messages)
		}
	}
}

// P3-4：未知工具的 ToolResult.ExitCode 应为 exitCodeNotSet（与被拒工具一致），
// 零值 0 会被事件层误读为"成功退出"。
func TestRun_UnknownToolExitCodeNotSet(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "missing", Args: "{}"}),
		textResponse("ok"),
	}}
	chat, stream := testClients(tr)
	var got *int
	hooks := LoopHooks{OnToolResult: func(name, callID string, r ToolResult) error {
		if name == "missing" {
			ec := r.ExitCode
			got = &ec
		}
		return nil
	}}
	if _, err := Run(context.Background(), chat, stream, LoopConfig{}, "x", hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got == nil {
		t.Fatal("OnToolResult not fired for missing tool")
	}
	if *got != exitCodeNotSet {
		t.Errorf("unknown tool ExitCode = %d, want %d (exitCodeNotSet)", *got, exitCodeNotSet)
	}
}

// panicTransport 在 RoundTrip 内 panic，模拟 Do/DoStream 响应解析路径（parseChatResponse /
// parseSSE）在畸形 payload 上 panic 的现实情形。
type panicTransport struct{ called bool }

func (p *panicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	p.called = true
	panic("boom")
}

// P3 LLM panic 兜底：callLLMOnce 的 recover 把 LLM 调用路径的 panic 转为 error，避免单次
// 坏响应崩进程（与防 tool panic 的 safeCall 对称）。无兜底则本用例会 panic 掉整个测试进程。
func TestRun_LLMCallPanicRecovered(t *testing.T) {
	tr := &panicTransport{}
	chat, stream := testClients(tr)
	_, err := Run(context.Background(), chat, stream, LoopConfig{Model: "m"}, "x", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error from recovered LLM panic, got nil")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("error should surface panic: %v", err)
	}
	if !tr.called {
		t.Error("transport was not invoked")
	}
}
