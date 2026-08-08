package miniagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/text"
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
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error on max iterations, got %v", err)
	}
	if res.Steps != maxIterations {
		t.Errorf("Steps = %d, want %d", res.Steps, maxIterations)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty (truncated)", res.Text)
	}
	// 每步 toolResponse 的 prompt_tokens=1；maxIterations 步主循环 + 总结步 → InputTokens 至少 maxIterations。
	// 弱断言只查非零会放过部分步骤漏记（如回归只记 1 token 仍绿），故查累加下界。
	if res.Usage.InputTokens < maxIterations {
		t.Errorf("usage 累加漏记: InputTokens=%d, want >= %d", res.Usage.InputTokens, maxIterations)
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
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 3}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
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
		llm := testClients(tr)
		res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: v}, "x", LoopHooks{}, nil)
		if err != nil {
			t.Fatalf("MaxIterations=%d: %v", v, err)
		}
		if res.Text != "ok" {
			t.Errorf("MaxIterations=%d: Text=%q want ok", v, res.Text)
		}
	}
}

// truncateHeadTail：头 n/4 + 尾 3n/4 + 中段 marker；短输入不截；n<=0 原样返回。
func TestTruncateHeadTail(t *testing.T) {
	s := "H" + strings.Repeat("m", 100) + "T" // 头 H，尾 T，中间噪声
	got := text.TruncateHeadTail(s, 40, "…[省略中间段]")
	if !strings.HasPrefix(got, "H") || !strings.HasSuffix(got, "T") {
		t.Errorf("应保留首尾字符: %q", got)
	}
	if !strings.Contains(got, "…[省略中间段]") {
		t.Errorf("应含中段 marker: %q", got)
	}
	// 头占 n/4=10，尾占 30。
	if !strings.HasPrefix(got, "H"+strings.Repeat("m", 9)) {
		t.Errorf("头部应占 n/4: %q", got[:min(len(got), 12)])
	}
	// 短输入不截断（无 marker）。
	if got := text.TruncateHeadTail("short", 100, "…"); strings.Contains(got, "…") {
		t.Errorf("短输入不应截断: %q", got)
	}
	// n<=0 原样返回。
	if got := text.TruncateHeadTail(s, 0, "…"); got != s {
		t.Errorf("n<=0 应原样返回")
	}
}

// C-3：reasoning 进入 transcript 并在下一步请求中以 reasoning_content 回灌。
func TestRun_ReasoningEntersHistory(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"think-step1","tool_calls":[{"id":"c1","type":"function","function":{"name":"q","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("done")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && m.Reasoning == "think-step1" {
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

// StepOutput{Commit:true, View:nil}（调用方误用）不应清空 transcript——
// applyBeforeLLM 的 Commit 路径用 toSend（已 nil 补救）而非裸 View。
func TestApplyBeforeLLM_CommitNilViewKeepsTranscript(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "keep"}}
	newMsgs := []Message{{Role: RoleUser, Content: "keep"}}
	total := &Usage{}
	compacted := false
	hooks := LoopHooks{
		BeforeLLM: func(context.Context, StepInput) (StepOutput, error) {
			return StepOutput{Commit: true, View: nil}, nil
		},
	}
	toSend, err := applyBeforeLLM(context.Background(), hooks, 1, &msgs, &newMsgs, total, &compacted, LoopConfig{})
	if err != nil {
		t.Fatalf("applyBeforeLLM: %v", err)
	}
	if len(toSend) != 1 || toSend[0].Content != "keep" {
		t.Errorf("toSend 应保留原 transcript: %+v", toSend)
	}
	if len(msgs) != 1 || msgs[0].Content != "keep" {
		t.Errorf("Commit+View=nil 不应清空 msgs: %+v", msgs)
	}
}

// 总结路径注入的 RoleSystem 引导消息不应进入 Result.Messages——
// 用临时 reqMsgs 发请求，transcript 始终干净（修复前 summaryReq 裸 append 进 msgs 泄漏）。
func TestRun_SummaryPathDoesNotLeakSystemPrompt(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	for i, m := range res.Messages {
		if m.Role == RoleSystem {
			t.Errorf("Result.Messages[%d] 不应含内部 RoleSystem 引导消息: %+v", i, m)
		}
	}
}

// 总结步返回 tool_calls（回落 FinishMaxIterations）时，该次调用 usage 仍应记账（与主路径 tool_calls 一致；
// 修复前 tool_calls 分支在累加前 return，致总结步 token 未进 total）。
func TestRun_SummaryFallbackAccountsUsage(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
	summary := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":200,"completion_tokens":20}}`
	tr := &fakeTransport{responses: []string{step1, summary}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
	}
	if res.Usage.InputTokens != 300 || res.Usage.OutputTokens != 30 {
		t.Errorf("Usage = %+v, want {300, 30}（step1 100+10 + 总结 200+20）", res.Usage)
	}
}

// 总结步 AfterLLM 抛错时 Steps 计 s（=iterLimit），与主路径 AfterLLM err 的 step-1 语义一致
// （recordStepUsage 未执行、usage 未记这步）。修复前误记 s+1。
func TestRun_SummaryAfterLLMErrorSteps(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("总结"),
	}}
	llm := testClients(tr)
	hooks := LoopHooks{
		AfterLLM: func(_ context.Context, step int, _ Response) error {
			if step == 2 { // 总结步 s+1
				return errors.New("afterllm boom")
			}
			return nil
		},
	}
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", hooks, nil)
	if err == nil {
		t.Fatal("expected AfterLLM error")
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1（总结 AfterLLM err 按 usage-未记语义计 s=iterLimit）", res.Steps)
	}
}
