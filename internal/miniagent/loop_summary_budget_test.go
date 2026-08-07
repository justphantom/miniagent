package miniagent

import (
	"context"
	"errors"
	"testing"
)

// 总结步（summarizeAtLimit）撞 maxIterations 后的 fallback 调用经 recordStepUsage 过 OnBudget（P1-3 修复）。
// 此前 fallback 手动累加 usage 绕过 OnBudget，退化路径可静默越 MaxTotalTokens。本测试锁定熔断闭合
// （回归审查 agent 指出的盲区：守卫代码正确但缺直接测试）。
func TestRun_SummarizeAtLimitOnBudgetExceeds(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	// 主循环 2 步 + 总结步都返回 tool_calls（总结步 tool_calls != 0 → fallback 走 recordStepUsage）。
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}), // 总结步 fallback
	}}
	llm := testClients(tr)
	// 每步 toolResponse prompt_tokens=1：主循环 2 步累计 input=2 < 3 通过；总结步累计 3 >= 3 熔断。
	hooks := LoopHooks{OnBudget: func(_ context.Context, _ int, _ BudgetInput, total *Usage) error {
		if total.InputTokens >= 3 {
			return ErrBudgetExceeded
		}
		return nil
	}}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 2}, "x", hooks, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("总结步 fallback 内 OnBudget 应熔断返回 ErrBudgetExceeded，got %v", err)
	}
}
