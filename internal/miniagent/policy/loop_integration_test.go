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

// defaultHooks replicates main.go's default hook assembly (OnLLMError + OnBudget + ShapeToolResult),
// so integration tests relying on the original core built-in policies (failure retry / usage estimation +
// budget circuit-break / tool-result truncation + persistence) can restore default behavior in one shot.
func defaultHooks(cfg miniagent.LoopConfig, logger *slog.Logger) miniagent.LoopHooks {
	return miniagent.LoopHooks{
		OnLLMError:      policy.NewDefaultOnLLMError(logger, 0),
		OnBudget:        policy.NewDefaultOnBudget(cfg.MaxTotalTokens, logger),
		ShapeToolResult: policy.NewDefaultShapeToolResult(cfg.Tools, cfg.ToolOutputDir, cfg.ToolOutputRetention, cfg.MaxToolResultChars, logger),
	}
}

// MaxTotalTokens: once cumulative tokens exceed the limit it terminates with miniagent.ErrBudgetExceeded (via the error path).
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
		t.Errorf("Steps = %d, want 1 (the over-limit call is counted)", res.Steps)
	}
}

// MaxTotalTokens<=0 means unlimited: normal multi-step completion.
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

// miniagent.Tool.ResultLimit drives history trimming: a high-limit tool's long result is trimmed to its limit when entering history,

// policy.NewDefaultOnBudget supplements a local estimation fallback when resp.Usage is all-zero (EstimateTokens counts the request side).
// Estimation was originally core built-in and is now externalized into the default hook; this test covers that factory directly to
// keep the fallback behavior from regressing.
func TestNewDefaultOnBudget_EstimatesZeroUsage(t *testing.T) {
	hook := policy.NewDefaultOnBudget(0, nil)
	total := &miniagent.Usage{}
	in := miniagent.BudgetInput{
		ToSend: []miniagent.Message{{Role: miniagent.RoleUser, Content: "a real prompt"}},
		Resp:   miniagent.Response{Usage: miniagent.Usage{}}, // all-zero → triggers local estimation
	}
	if err := hook(context.Background(), 1, in, total); err != nil {
		t.Fatalf("OnBudget: %v", err)
	}
	if total.InputTokens == 0 {
		t.Errorf("OnBudget did not estimate zero-usage; expected local estimate fallback, got %+v", total)
	}
}

// P1-5-b: with all-zero usage, local estimation still triggers the MaxTotalTokens budget circuit-break.
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

// miniagent.Tool.ResultLimit drives history trimming: a high-limit tool's long result is trimmed to its limit
// when entering history, not the default 2000.
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
		if !strings.Contains(m.Content, "truncated") {
			t.Errorf("trim marker missing: len=%d", len(m.Content))
		}
	}
}

// C-2: context-overflow downgrade. First 400(context_length) → tighten history → retry this step successfully.
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

// C-2: retry still over the limit → downgrade only once then propagate miniagent.ErrContextLength, no infinite retry.
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

// §P1-A: when tool output exceeds the limit and ToolOutputDir is enabled, the full text is persisted to disk, the history Content
// contains a path hint, and the file content == the original text.
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
	if !strings.Contains(toolMsg.Content, "saved") || !strings.Contains(toolMsg.Content, dir) {
		t.Errorf("tool content should contain a path hint (saved + directory): len=%d", len(toolMsg.Content))
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("should persist exactly 1 file: entries=%d err=%v", len(entries), err)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if string(b) != big {
		t.Errorf("persisted file content should == original text: got len=%d want=%d", len(b), len(big))
	}
}

// §P1-A: ToolOutputDir empty (disabled by default) → no persistence IO across the whole chain, behavior equals
// policy.TrimForHistory hard truncation (regression guard).
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
	if strings.Contains(toolMsg.Content, "saved") {
		t.Errorf("when disabled should not contain a path hint: %q", toolMsg.Content)
	}
	want := policy.TrimForHistory(big, 0, false)
	if toolMsg.Content != want {
		t.Errorf("when disabled should == policy.TrimForHistory hard truncation: got len=%d want len=%d", len(toolMsg.Content), len(want))
	}
}

// §P1-A: for a SplitTruncate=true tool, the preview after persistence still contains the key tail line
// (the head-1/4 + tail-3/4 semantics are not rewritten by the store).
func TestRun_ToolOutputPreservesSplit(t *testing.T) {
	head := strings.Repeat("head-line\n", 700) // ~7000-char head
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
		t.Errorf("split preview should retain the tail FAIL line: %q...(len %d)", toolMsg.Content[:min(60, len(toolMsg.Content))], len(toolMsg.Content))
	}
	if !strings.Contains(toolMsg.Content, "saved") {
		t.Errorf("over the limit should trigger a persistence path hint")
	}
}

// The summary step that hits the iteration limit goes through OnBudget: cumulative exceeding MaxTotalTokens should circuit-break
// (before the fix the summary step bypassed OnBudget and returned text directly).
// Per-step tool usage 100+10=110; two steps cumulative 220<250 (main path does not break), the summary step adds +110=330>250 triggering the break.
func TestRun_SummaryStepEnforcesBudget(t *testing.T) {
	tool := miniagent.Tool{Name: "loop", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "x"} }}
	toolStep := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
	summaryStep := `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
	tr := &looptest.FakeTransport{Responses: []string{toolStep, toolStep, summaryStep}}
	llm := looptest.NewFakeLLM(tr)
	cfg := miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, MaxIterations: 2, MaxTotalTokens: 250}
	_, err := miniagent.Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if !errors.Is(err, miniagent.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want miniagent.ErrBudgetExceeded (summary step should circuit-break via OnBudget)", err)
	}
}

// OnLLMError recovery retry path: if it triggers a thinking downgrade, miniagent.Run must capture downgraded and set ThinkingDowngraded
// (fix: the original retry call `resp, _, err = ...` discarded downgraded, so the interactive layer would resend the original value
// next turn and hit 400 again).
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
		t.Errorf("ThinkingDowngraded = false, want true (retry-path downgrade should be captured)")
	}
	if tr.Calls != 3 {
		t.Errorf("calls = %d, want 3 (ctx-len + thinking-unsupported + ok)", tr.Calls)
	}
	// The first retry still carries thinking (the downgrade happens inside the retry); the request after the downgrade does not.
	if !strings.Contains(tr.Bodies[1], "reasoning_effort") {
		t.Errorf("first retry should still carry thinking: %s", tr.Bodies[1])
	}
	if strings.Contains(tr.Bodies[2], "reasoning_effort") {
		t.Errorf("after downgrade thinking should be removed: %s", tr.Bodies[2])
	}
}

// P1-5: when usage is all-zero (endpoint returns no usage), Run uses the local estimation fallback and keeps running, while exposing a warn.
// This tests policy.NewDefaultOnBudget's zero-usage local estimation + warn log.
func TestRun_ZeroUsageWarns(t *testing.T) {
	// Response has no usage field → looptest.ParseChatResponse yields a zero Usage (a realistic case common with streaming endpoints).
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
