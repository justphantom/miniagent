package miniagent

import (
	"context"
	"strings"
	"testing"
)

// 阶段 3：迭代上限后注入总结 prompt，LLM 返回文本 → finishStop，steps=maxIterations+1。
func TestRun_SummaryInjectionSucceeds(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("总结完成"),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != finishStop {
		t.Errorf("Finish = %q, want %q", res.Finish, finishStop)
	}
	if res.Text != "总结完成" {
		t.Errorf("Text = %q, want '总结完成'", res.Text)
	}
	if res.Steps != 2 { // 1 步工具 + 1 步总结
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
	if tr.calls != 2 {
		t.Errorf("LLM calls = %d, want 2", tr.calls)
	}
	// 第二次请求 body 应含 summary request prompt。
	secondBody := tr.bodies[1]
	if !strings.Contains(secondBody, summaryRequestPrompt) {
		t.Errorf("second request missing summary prompt: %s", secondBody)
	}
}

// 阶段 3：迭代上限后注入总结 prompt，LLM 仍请求工具 → 回落 finishMaxIterations。
func TestRun_SummaryInjectionFallsBack(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		toolResponse(ToolCall{ID: "c2", Name: "loop", Args: "{}"}),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != finishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, finishMaxIterations)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1（回落，不累计额外步）", res.Steps)
	}
	// 但 summary request 仍注入了历史（messages 含该 system 消息）。
	hasSummary := false
	for _, m := range res.Messages {
		if m.Role == RoleSystem && strings.Contains(m.Content, summaryRequestPrompt) {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		t.Error("summary request not in messages")
	}
}

// 阶段 3：迭代上限后注入自定义 summary request prompt（通过 LoopConfig 配置）。
func TestRun_SummaryRequestPromptConfigurable(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("自定义总结"),
	}}
	customPrompt := "这是自定义总结引导"
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{
		Tools:          []Tool{tool},
		MaxIterations:  1,
		SummaryRequest: customPrompt,
	}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "自定义总结" {
		t.Errorf("Text = %q, want '自定义总结'", res.Text)
	}
	// 验证使用了自定义 prompt 而非默认值。
	secondBody := tr.bodies[1]
	if !strings.Contains(secondBody, customPrompt) {
		t.Errorf("second request missing custom summary prompt: %s", secondBody)
	}
	if strings.Contains(secondBody, summaryRequestPrompt) {
		t.Errorf("second request should not contain default summary prompt: %s", secondBody)
	}
}
