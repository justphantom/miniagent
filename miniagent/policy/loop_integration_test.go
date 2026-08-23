package policy_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent/looptest"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/policy"
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
		if len(m.Content) > 8200 || len(m.Content) <= miniagent.MaxToolResultInHistory {
			t.Errorf("tool content not trimmed by ResultLimit: len=%d (want (%d, 8200])", len(m.Content), miniagent.MaxToolResultInHistory)
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
