package miniagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRun_AssistantTextPreservedWithToolCalls(t *testing.T) {
	resp := `{"choices":[{"message":{"role":"assistant","content":"先查一下","tool_calls":[{"id":"1","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tr := &fakeTransport{responses: []string{resp, textResponse("done")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{{Name: "read", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "ok"} }}}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var asst string
	found := false
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			found, asst = true, m.Content
		}
	}
	if !found {
		t.Fatal("未找到带 tool_calls 的 assistant 消息")
	}
	if asst != "先查一下" {
		t.Errorf("assistant 含 tool_calls 时 Content = %q, want %q（resp.Text 不应丢失，R4-2）", asst, "先查一下")
	}
}

func TestRun_TextOnlyReturnsImmediately(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("hello world")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", System: "be brief"}, "hi", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d", tr.calls)
	}
	if !strings.Contains(tr.lastBody, `"role":"system"`) || !strings.Contains(tr.lastBody, "be brief") {
		t.Errorf("system not sent: %s", tr.lastBody)
	}
}

func TestRun_ReActToolThenText(t *testing.T) {
	called := false
	tool := Tool{
		Name: "echo",
		Call: func(_ context.Context, args string) ToolResult {
			called = true
			return ToolResult{Output: "echoed " + args}
		},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	var uses []string
	onToolUse := func(name, input string) error { uses = append(uses, name); return nil }
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{OnToolUse: onToolUse}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if !called {
		t.Error("tool not called")
	}
	// 只通知一次 tool_use（终态文本由 Result 携带）。
	if len(uses) != 1 || uses[0] != "echo" {
		t.Errorf("uses = %v", uses)
	}
}

func TestRun_UnknownToolYieldsErrorResult(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "missing", Args: "{}"}),
		textResponse("ok"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestRun_ToolPanicRecovered(t *testing.T) {
	tool := Tool{
		Name: "boom",
		Call: func(context.Context, string) ToolResult { panic("boom") },
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "boom", Args: "{}"}),
		textResponse("recovered"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
}

func TestRun_LLMErrorPropagates(t *testing.T) {
	// 3 次 503：重试 testMaxRetries 次后仍失败，最终把错误上抛。
	tr := &fakeTransport{statuses: []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}}
	llm := testClients(tr)
	_, err := Run(context.Background(), llm, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if tr.calls != 1+testMaxRetries {
		t.Errorf("calls = %d, want %d (1 + testMaxRetries)", tr.calls, 1+testMaxRetries)
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "hi", LoopHooks{}, nil); err == nil {
		t.Fatal("expected error")
	}
}

// 一步内的多个 tool_call 并发用例见 loop_concurrency_test.go。

// 多步 ReAct：第一步工具结果回灌，第二步拿到最终文本。
func TestRun_MultiStepReAct(t *testing.T) {
	tool := Tool{Name: "query", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "data-42"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "query", Args: "{}"}),
		toolResponse(ToolCall{ID: "c2", Name: "query", Args: "{}"}),
		textResponse("final answer"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Text != "final answer" {
		t.Errorf("Text = %q", res.Text)
	}
}

// P2-2：端点不支持 thinking 时，跨步仅首步降级一次；后续步直接走无 thinking，不再撞 400。
func TestRun_ThinkingDowngradePersistsAcrossSteps(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	thinkErr := `{"error":{"message":"unknown parameter: reasoning_effort"}}`
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusOK, http.StatusOK},
		responses: []string{thinkErr, toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}), textResponse("done")},
	}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}, Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	// 无降级固化时 step2 会重发 thinking → 多一次 400 + 重试（共 4 次）；固化后共 3 次。
	if len(tr.bodies) != 3 {
		t.Fatalf("calls = %d, want 3 (降级仅首步一次，step2 应直发无 thinking)", len(tr.bodies))
	}
	for i, b := range tr.bodies {
		has := strings.Contains(b, "reasoning_effort")
		if i == 0 && !has {
			t.Errorf("body[%d] 应带 thinking（首次探测）: %s", i, b)
		}
		if i != 0 && has {
			t.Errorf("body[%d] 不应带 thinking（降级应已固化）: %s", i, b)
		}
	}
	// Fix 5：降级发生 → result.ThinkingDowngraded=true（交互层据此清 baseCfg，审查 P2 跨轮固化）。
	if !res.ThinkingDowngraded {
		t.Errorf("ThinkingDowngraded should be true after a downgrade occurred")
	}
}

// 极简模式（BeforeLLM=nil）：核心不做任何上下文管理——巨大历史原样发送，不压缩、Compacted=false。
// 这是「核心极简 + 压缩外挂」的契约证明：不挂 NewCompaction 即得无压缩 agent。
func TestRun_NilBeforeLLMIsMinimalNoCompaction(t *testing.T) {
	big := strings.Repeat("x", 1000)
	hist := make([]Message, 20)
	for i := range hist {
		hist[i] = Message{Role: RoleUser, Content: big}
	}
	tr := &fakeTransport{responses: []string{textResponse("done")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{History: hist}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Compacted {
		t.Error("minimal mode (nil BeforeLLM) must not compact")
	}
	if tr.calls != 1 {
		t.Errorf("minimal mode: want 1 LLM call, got %d", tr.calls)
	}
	// 全部历史原样进请求体（无压缩、无裁剪）——压缩了就不会含完整 big blob。
	if !strings.Contains(tr.lastBody, big) {
		t.Errorf("minimal mode should send history verbatim, body lacks big blob: %s", tr.lastBody)
	}
}

// OnBudget 返回 ErrBudgetExceeded → Run 立即终止并上抛该 error（预算外挂的熔断契约）。
func TestRun_OnBudgetExceedsStops(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	// 响应 usage={1,1}（见 toolResponse）：首步累计 total={1,1}，阈值 1 → 熔断。
	hooks := LoopHooks{OnBudget: func(_ context.Context, _ int, _ BudgetInput, total *Usage) error {
		if total.InputTokens+total.OutputTokens > 1 {
			return ErrBudgetExceeded
		}
		return nil
	}}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

// appendMsg 打戳：Ts==0 自动打戳（>0）；显式 Ts 保留。
func TestAppendMsg_Timestamp(t *testing.T) {
	var msgs, newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: "auto"})
	if msgs[0].Ts == 0 {
		t.Error("Ts==0 应被 appendMsg 自动打戳为 >0")
	}
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: "manual", Ts: 42})
	if msgs[1].Ts != 42 {
		t.Errorf("显式 Ts 应保留: got %d, want 42", msgs[1].Ts)
	}
}
