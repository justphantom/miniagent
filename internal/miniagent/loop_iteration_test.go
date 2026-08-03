package miniagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// 工具调用永不停：每次都返回 tool_calls，触发 maxIterations 上限。
// 终止信号由 Finish=max_iterations 表达（Steps=maxIterations + 空 Text）。
func TestRun_MaxIterationsReturnsBurnedUsage(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error on max iterations, got %v", err)
	}
	if res.Steps != maxIterations {
		t.Errorf("Steps = %d, want %d", res.Steps, maxIterations)
	}
	if res.Finish != finishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, finishMaxIterations)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty (truncated)", res.Text)
	}
	if res.Usage.InputTokens == 0 {
		t.Error("expected non-zero usage accounting")
	}
}

// MaxIterations 可覆盖默认上限：设为 3 则第 3 步撞顶（不受包默认 20 影响）。
func TestRun_MaxIterationsOverride(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, 10)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, MaxIterations: 3}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Finish != finishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, finishMaxIterations)
	}
}

// MaxIterations<=0 回退默认值：第 2 步给最终文本，验证未被误解析为极小上限。
func TestRun_MaxIterationsNonPositiveUsesDefault(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	for _, v := range []int{0, -1} {
		tr := &fakeTransport{responses: []string{
			toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
			textResponse("ok"),
		}}
		chat, stream := testClients(tr)
		res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, MaxIterations: v}, "x", LoopHooks{}, nil)
		if err != nil {
			t.Fatalf("MaxIterations=%d: %v", v, err)
		}
		if res.Text != "ok" {
			t.Errorf("MaxIterations=%d: Text=%q want ok", v, res.Text)
		}
	}
}

// trimForHistory：显式 limit 装裁到该值；limit<=0 用默认 maxToolResultInHistory。
func TestTrimForHistory_PerLimit(t *testing.T) {
	big := strings.Repeat("x", 10000)
	got := trimForHistory(big, 8000)
	if len(got) <= 8000 || !strings.Contains(got, "截断") {
		t.Errorf("limit=8000: len=%d, marker missing: %q", len(got), got[:min(len(got), 40)])
	}
	got0 := trimForHistory(big, 0)
	// 默认裁到 maxToolResultInHistory：长度应略大于该值（含 marker），远小于 8000。
	if len(got0) <= maxToolResultInHistory || len(got0) >= 8000 {
		t.Errorf("limit=0: len=%d, want in (%d, 8000)", len(got0), maxToolResultInHistory)
	}
}

// MaxTotalTokens：累计 token 超限即以 ErrBudgetExceeded 终止（走 error 路径）。
func TestRun_BudgetExceeded(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	bigUsage := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{bigUsage, bigUsage}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, MaxTotalTokens: 1000}, "x", LoopHooks{}, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1（超限的那次调用计入）", res.Steps)
	}
}

// MaxTotalTokens<=0 不限：正常多步完成。
func TestRun_BudgetZeroUnlimited(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("ok"),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, MaxTotalTokens: 0}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
}

// Tool.ResultLimit 驱动历史裁剪：高限工具的长结果入历史时按其 limit 裁，
// 而非默认 2000。
func TestRun_ToolResultLimitUsedInHistory(t *testing.T) {
	long := strings.Repeat("y", 9000)
	tool := Tool{
		Name:        "bigread",
		ResultLimit: maxFileResultInHistory,
		Call:        func(context.Context, string) ToolResult { return ToolResult{Output: long} },
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "bigread", Args: "{}"}),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role != roleTool {
			continue
		}
		if len(m.Content) > 8200 || len(m.Content) <= maxToolResultInHistory {
			t.Errorf("tool content not trimmed by ResultLimit: len=%d (want (%d, 8200])", len(m.Content), maxToolResultInHistory)
		}
		if !strings.Contains(m.Content, "截断") {
			t.Errorf("trim marker missing: len=%d", len(m.Content))
		}
	}
}

// C-3：reasoning 进入 transcript 并在下一步请求中以 reasoning_content 回灌。
func TestRun_ReasoningEntersHistory(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"think-step1","tool_calls":[{"id":"c1","type":"function","function":{"name":"q","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("done")}}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range res.Messages {
		if m.Role == roleAssistant && m.Reasoning == "think-step1" {
			found = true
		}
	}
	if !found {
		t.Errorf("reasoning not in transcript: %+v", res.Messages)
	}
	if !strings.Contains(tr.lastBody, "reasoning_content") || !strings.Contains(tr.lastBody, "think-step1") {
		t.Errorf("reasoning not sent back in next request: %s", tr.lastBody)
	}
}

// C-2：context 超限降级。首次 400(context_length) → 收紧历史 → 重试本步成功。
func TestRun_ContextLengthFallbackOnce(t *testing.T) {
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusOK},
		responses: []string{`{"error":{"message":"maximum context length exceeded"}}`, textResponse("recovered")},
	}
	chat, stream := testClients(tr)
	res, err := Run(context.Background(), chat, stream, LoopConfig{}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q, want recovered", res.Text)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want 2 (1 initial + 1 fallback)", tr.calls)
	}
}

// C-2：重试仍超限 → 只降级一次后上抛 ErrContextLength，不无限重试。
func TestRun_ContextLengthFallbackStillTooLong(t *testing.T) {
	body := `{"error":{"message":"maximum context length exceeded"}}`
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusBadRequest},
		responses: []string{body, body},
	}
	chat, stream := testClients(tr)
	_, err := Run(context.Background(), chat, stream, LoopConfig{}, "x", LoopHooks{}, nil)
	if !errors.Is(err, ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength", err)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want 2 (only one fallback)", tr.calls)
	}
}
