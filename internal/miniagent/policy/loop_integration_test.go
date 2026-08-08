package policy_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/looptest"
	"github.com/justphantom/miniagent/internal/miniagent/policy"
)

// defaultHooks 复刻 main.go 的默认钩子组装（OnLLMError + OnBudget + ShapeToolResult），
// 供依赖原核心内置策略（失败重试/用量估算+预算熔断/工具结果截断+落盘）的集成测试一键恢复默认行为。
func defaultHooks(cfg miniagent.LoopConfig, logger *slog.Logger) miniagent.LoopHooks {
	return miniagent.LoopHooks{
		OnLLMError:      policy.NewDefaultOnLLMError(logger, 0),
		OnBudget:        policy.NewDefaultOnBudget(cfg.MaxTotalTokens, logger),
		ShapeToolResult: policy.NewDefaultShapeToolResult(cfg.Tools, cfg.ToolOutputDir, cfg.ToolOutputRetention, cfg.MaxToolResultChars, logger),
	}
}

// MaxTotalTokens：累计 token 超限即以 miniagent.ErrBudgetExceeded 终止（走 error 路径）。
func TestRun_BudgetExceeded(t *testing.T) {
	tool := miniagent.Tool{Name: "loop", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "x"} }}
	bigUsage := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`
	tr := &looptest.FakeTransport{Responses: []string{bigUsage, bigUsage}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, MaxTotalTokens: 1000}
	res, err := miniagent.Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if !errors.Is(err, miniagent.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want miniagent.ErrBudgetExceeded", err)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1（超限的那次调用计入）", res.Steps)
	}
}

// MaxTotalTokens<=0 不限：正常多步完成。
func TestRun_BudgetZeroUnlimited(t *testing.T) {
	tool := miniagent.Tool{Name: "q", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "x"} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		looptest.TextResponse("ok"),
	}}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, MaxTotalTokens: 0}, "x", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
}

// miniagent.Tool.ResultLimit 驱动历史裁剪：高限工具的长结果入历史时按其 limit 裁，

// policy.NewDefaultOnBudget 在 resp.Usage 全零时补本地估算 fallback（EstimateTokens 计请求侧）。
// 估算原属核心内置，现外挂到默认钩子；本测试直接覆盖该工厂，保 fallback 行为不丢。
func TestNewDefaultOnBudget_EstimatesZeroUsage(t *testing.T) {
	hook := policy.NewDefaultOnBudget(0, nil)
	total := &miniagent.Usage{}
	in := miniagent.BudgetInput{
		ToSend: []miniagent.Message{{Role: miniagent.RoleUser, Content: "a real prompt"}},
		Resp:   miniagent.Response{Usage: miniagent.Usage{}}, // 全零 → 触发本地估算
	}
	if err := hook(context.Background(), 1, in, total); err != nil {
		t.Fatalf("OnBudget: %v", err)
	}
	if total.InputTokens == 0 {
		t.Errorf("OnBudget did not estimate zero-usage; expected local estimate fallback, got %+v", total)
	}
}

// P1-5-b：usage 全零时本地估算仍触发 MaxTotalTokens 预算熔断。
func TestRun_ZeroUsageBudgetEnforced(t *testing.T) {
	noUsage := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	tr := &looptest.FakeTransport{Responses: []string{noUsage}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Model: "m", MaxTotalTokens: 100}
	_, err := miniagent.Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if !errors.Is(err, miniagent.ErrBudgetExceeded) {
		t.Fatalf("expected miniagent.ErrBudgetExceeded, got %v", err)
	}
}

// miniagent.Tool.ResultLimit 驱动历史裁剪：高限工具的长结果入历史时按其 limit 裁，
// 而非默认 2000。
func TestRun_ToolResultLimitUsedInHistory(t *testing.T) {
	long := strings.Repeat("y", 9000)
	tool := miniagent.Tool{
		Name:        "bigread",
		ResultLimit: 8000,
		Call:        func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: long} },
	}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "bigread", Args: "{}"}),
		looptest.TextResponse("done"),
	}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}}
	res, err := miniagent.Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role != miniagent.RoleTool {
			continue
		}
		if len(m.Content) > 8200 || len(m.Content) <= policy.MaxToolResultInHistory {
			t.Errorf("tool content not trimmed by ResultLimit: len=%d (want (%d, 8200])", len(m.Content), policy.MaxToolResultInHistory)
		}
		if !strings.Contains(m.Content, "截断") {
			t.Errorf("trim marker missing: len=%d", len(m.Content))
		}
	}
}

// C-2：context 超限降级。首次 400(context_length) → 收紧历史 → 重试本步成功。
func TestRun_ContextLengthFallbackOnce(t *testing.T) {
	tr := &looptest.FakeTransport{
		Statuses:  []int{http.StatusBadRequest, http.StatusOK},
		Responses: []string{`{"error":{"message":"maximum context length exceeded"}}`, looptest.TextResponse("recovered")},
	}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{}
	res, err := miniagent.Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q, want recovered", res.Text)
	}
	if tr.Calls != 2 {
		t.Errorf("calls = %d, want 2 (1 initial + 1 fallback)", tr.Calls)
	}
}

// C-2：重试仍超限 → 只降级一次后上抛 miniagent.ErrContextLength，不无限重试。
func TestRun_ContextLengthFallbackStillTooLong(t *testing.T) {
	body := `{"error":{"message":"maximum context length exceeded"}}`
	tr := &looptest.FakeTransport{
		Statuses:  []int{http.StatusBadRequest, http.StatusBadRequest},
		Responses: []string{body, body},
	}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{}
	_, err := miniagent.Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if !errors.Is(err, miniagent.ErrContextLength) {
		t.Fatalf("err = %v, want miniagent.ErrContextLength", err)
	}
	if tr.Calls != 2 {
		t.Errorf("calls = %d, want 2 (only one fallback)", tr.Calls)
	}
}

// §P1-A：工具输出超 limit 且 ToolOutputDir 启用时，全文落盘、历史 Content 含路径提示、文件内容==原文。
func TestRun_ToolOutputStoredOnTruncation(t *testing.T) {
	big := strings.Repeat("x", 5000) // > policy.MaxToolResultInHistory(4000)
	tool := miniagent.Tool{Name: "bigtool", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: big} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "bigtool", Args: "{}"}),
		looptest.TextResponse("done"),
	}}
	llm := looptest.NewFakeLLM(tr)
	dir := t.TempDir()
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, ToolOutputDir: dir}
	res, err := miniagent.Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	toolMsg := looptest.LastToolMessage(t, res.NewMessages)
	if !strings.Contains(toolMsg.Content, "已保存") || !strings.Contains(toolMsg.Content, dir) {
		t.Errorf("tool content 应含路径提示（已保存 + 目录）: len=%d", len(toolMsg.Content))
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("应恰好落盘 1 个文件: entries=%d err=%v", len(entries), err)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if string(b) != big {
		t.Errorf("落盘文件内容应 == 原文: got len=%d want=%d", len(b), len(big))
	}
}

// §P1-A：ToolOutputDir 空（默认禁用）→ 全链路无落盘 IO，行为等同 policy.TrimForHistory 硬截断（回归保护）。
func TestRun_ToolOutputDisabledByDefault(t *testing.T) {
	big := strings.Repeat("x", 5000)
	tool := miniagent.Tool{Name: "bigtool", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: big} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "bigtool", Args: "{}"}),
		looptest.TextResponse("done"),
	}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}}
	res, err := miniagent.Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	toolMsg := looptest.LastToolMessage(t, res.NewMessages)
	if strings.Contains(toolMsg.Content, "已保存") {
		t.Errorf("禁用时不应含路径提示: %q", toolMsg.Content)
	}
	want := policy.TrimForHistory(big, 0, false)
	if toolMsg.Content != want {
		t.Errorf("禁用时应 == policy.TrimForHistory 硬截断: got len=%d want len=%d", len(toolMsg.Content), len(want))
	}
}

// §P1-A：SplitTruncate=true 的工具，落盘后预览仍含尾部关键行（头1/4+尾3/4 语义不被 store 改写）。
func TestRun_ToolOutputPreservesSplit(t *testing.T) {
	head := strings.Repeat("head-line\n", 700) // ~7000 字符头部
	tail := "FINAL_FAIL_LINE\n"
	big := head + tail
	tool := miniagent.Tool{Name: "shelltool", SplitTruncate: true, Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: big} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "shelltool", Args: "{}"}),
		looptest.TextResponse("done"),
	}}
	llm := looptest.NewFakeLLM(tr)
	dir := t.TempDir()
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, ToolOutputDir: dir}
	res, err := miniagent.Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	toolMsg := looptest.LastToolMessage(t, res.NewMessages)
	if !strings.Contains(toolMsg.Content, "FINAL_FAIL_LINE") {
		t.Errorf("split 预览应保留尾部 FAIL 行: %q...(len %d)", toolMsg.Content[:min(60, len(toolMsg.Content))], len(toolMsg.Content))
	}
	if !strings.Contains(toolMsg.Content, "已保存") {
		t.Errorf("超限应触发落盘路径提示")
	}
}

// 撞迭代上限的总结步走 OnBudget：累计超 MaxTotalTokens 应熔断（修复前总结步绕过 OnBudget 直接返回文本）。
// 每步 tool usage 100+10=110；两步累计 220<250（主路径不熔断），总结步再 +110=330>250 触发熔断。
func TestRun_SummaryStepEnforcesBudget(t *testing.T) {
	tool := miniagent.Tool{Name: "loop", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "x"} }}
	toolStep := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
	summaryStep := `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
	tr := &looptest.FakeTransport{Responses: []string{toolStep, toolStep, summaryStep}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, MaxIterations: 2, MaxTotalTokens: 250}
	_, err := miniagent.Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if !errors.Is(err, miniagent.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want miniagent.ErrBudgetExceeded（总结步应走 OnBudget 熔断）", err)
	}
}

// OnLLMError 恢复重试路径若触发 thinking 降级，miniagent.Run 应捕获 downgraded 并置位 ThinkingDowngraded
// （修复：原重试调用 `resp, _, err = ...` 丢弃 downgraded，交互层下轮会重传原值再撞 400）。
func TestRun_RetryPathCapturesDowngrade(t *testing.T) {
	tr := &looptest.RecordingTransport{Plan: []looptest.TransportResp{
		{Status: http.StatusBadRequest, Body: `{"error":{"message":"maximum context length exceeded"}}`},
		{Status: http.StatusBadRequest, Body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{Status: http.StatusOK, Body: looptest.TextResponse("recovered")},
	}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &miniagent.ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}
	hooks := miniagent.LoopHooks{OnLLMError: policy.NewDefaultOnLLMError(nil, 0)}
	res, err := miniagent.Run(context.Background(), llm, cfg, "x", hooks, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q, want recovered", res.Text)
	}
	if !res.ThinkingDowngraded {
		t.Errorf("ThinkingDowngraded = false, want true（重试路径降级应被捕获）")
	}
	if tr.Calls != 3 {
		t.Errorf("calls = %d, want 3（ctx-len + thinking-unsupported + ok）", tr.Calls)
	}
	// 重试首次仍带 thinking（降级发生在重试内部），降级后那次请求不带。
	if !strings.Contains(tr.Bodies[1], "reasoning_effort") {
		t.Errorf("重试首次应仍带 thinking: %s", tr.Bodies[1])
	}
	if strings.Contains(tr.Bodies[2], "reasoning_effort") {
		t.Errorf("降级后应去 thinking: %s", tr.Bodies[2])
	}
}

// P1-5：usage 全零（端点不返回 usage）时 Run 用本地估算 fallback 并继续运行，同时 warn 暴露。
// 测的是 policy.NewDefaultOnBudget 的零 usage 本地估算 + warn 日志。
func TestRun_ZeroUsageWarns(t *testing.T) {
	// 响应不含 usage 字段 → looptest.ParseChatResponse 得零值 Usage（流式端点常见的现实情形）。
	noUsage := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	tr := &looptest.FakeTransport{Responses: []string{noUsage}}
	llm := looptest.NewFakeLLM(tr)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := miniagent.LoopConfig{Model: "m", MaxTotalTokens: 10000}
	res, err := miniagent.Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, logger), logger)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("Text = %q, want hi", res.Text)
	}
	if !strings.Contains(buf.String(), "partial/zero usage") {
		t.Errorf("expected warn about missing usage, got logs: %s", buf.String())
	}
}
