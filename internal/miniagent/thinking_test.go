package miniagent

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestBuildChatBody_ThinkingLevelWritten(t *testing.T) {
	body, err := testBuildChatBody(Request{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning_effort":"medium"`) {
		t.Errorf("thinking level not written: %s", body)
	}
}

func TestBuildChatBody_ThinkingOffOmitted(t *testing.T) {
	for _, lvl := range []string{"", ThinkingOff} {
		body, _ := testBuildChatBody(Request{Model: "m", ThinkingLevel: lvl})
		if strings.Contains(string(body), "reasoning_effort") {
			t.Errorf("level %q should omit thinking: %s", lvl, body)
		}
	}
}

func TestBuildChatBody_ThinkingMappingOverrides(t *testing.T) {
	req := Request{
		Model:         "m",
		ThinkingLevel: "medium",
		Thinking:      &ThinkingMapping{Field: "effort", Map: map[string]string{"medium": "high"}},
	}
	body, _ := testBuildChatBody(req)
	if !strings.Contains(string(body), `"effort":"high"`) {
		t.Errorf("mapping override not applied: %s", body)
	}
	if strings.Contains(string(body), "reasoning_effort") {
		t.Errorf("default field should be replaced: %s", body)
	}
}

// 400 含 thinking 特征 → callLLMWithDowngrade 去 thinking 重试一次（审查 v2 #7）。
func TestCallLLM_Thinking400Downgrade(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{status: http.StatusOK, body: textResponse("ok")},
	}}
	llm := testClients(tr)
	resp, _, _, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("callLLMWithDowngrade: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	if tr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (downgrade retry)", tr.calls)
	}
	if !strings.Contains(tr.bodies[0], `"reasoning_effort":"medium"`) {
		t.Errorf("first body should carry thinking: %s", tr.bodies[0])
	}
	if strings.Contains(tr.bodies[1], "reasoning_effort") {
		t.Errorf("second body should drop thinking: %s", tr.bodies[1])
	}
}

// 非 thinking 的 400 不触发降级（无 thinking 发送时直接上抛）。
func TestCallLLM_Plain400NoDowngrade(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
	}}
	llm := testClients(tr)
	_, _, _, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m"}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error for plain 400")
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d, want 1 (no downgrade without thinking)", tr.calls)
	}
}

// 总结步触发 thinking 降级时 Result.ThinkingDowngraded 应置位（与主路径/C 重试同款；
// 修复前闭包 `resp2, _, err2` 忽略 downgraded，致交互层下轮重传原 thinking 再撞 400）。
func TestRun_SummaryStepCapturesDowngrade(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusOK, body: toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})},
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{status: http.StatusOK, body: textResponse("总结完成")},
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1, ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "总结完成" {
		t.Errorf("Text = %q, want 总结完成", res.Text)
	}
	if !res.ThinkingDowngraded {
		t.Errorf("ThinkingDowngraded = false, want true（总结步降级应被捕获）")
	}
	if tr.calls != 3 {
		t.Errorf("calls = %d, want 3（step1 tool + 总结 thinking 400 + 总结降级 ok）", tr.calls)
	}
}
