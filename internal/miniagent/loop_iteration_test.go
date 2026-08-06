package miniagent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 3}, "x", LoopHooks{}, nil)
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

// trimForHistory：显式 limit 装裁到该值；limit<=0 用默认 maxToolResultInHistory。split=false 走 head-only。
func TestTrimForHistory_PerLimit(t *testing.T) {
	big := strings.Repeat("x", 10000)
	got := trimForHistory(big, 8000, false)
	if len(got) <= 8000 || !strings.Contains(got, "截断") {
		t.Errorf("limit=8000: len=%d, marker missing: %q", len(got), got[:min(len(got), 40)])
	}
	got0 := trimForHistory(big, 0, false)
	// 默认裁到 maxToolResultInHistory：长度应略大于该值（含 marker），远小于 8000。
	if len(got0) <= maxToolResultInHistory || len(got0) >= 8000 {
		t.Errorf("limit=0: len=%d, want in (%d, 8000)", len(got0), maxToolResultInHistory)
	}
}

// trimForHistory split=true（shell/grep）：头尾分段截断，保留尾部错误结论标记，总长落在 limit 附近。
func TestTrimForHistory_SplitKeepsTail(t *testing.T) {
	// 头部上下文 + 大段中间噪声 + 尾部错误结论。head-only 会丢掉 FAIL 行。
	body := "CMD: build\n" + strings.Repeat("log\n", 2000) + "FAIL: exit status 1"
	got := trimForHistory(body, 4000, true)
	if !strings.Contains(got, "省略中间段") {
		t.Errorf("split 应含中段省略标记: %q", got[:min(len(got), 60)])
	}
	if !strings.Contains(got, "FAIL: exit status 1") {
		t.Errorf("split 应保留尾部错误结论: tail=%q", got[max(0, len(got)-80):])
	}
	if !strings.Contains(got, "CMD: build") {
		t.Errorf("split 应保留头部上下文: head=%q", got[:min(len(got), 40)])
	}
	if len(got) > 4200 { // 头 n/4 + 尾 3n/4 + marker，应略超 limit
		t.Errorf("split 总长应接近 limit: len=%d", len(got))
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

// MaxTotalTokens：累计 token 超限即以 ErrBudgetExceeded 终止（走 error 路径）。
func TestRun_BudgetExceeded(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	bigUsage := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{bigUsage, bigUsage}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxTotalTokens: 1000}
	res, err := Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
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
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxTotalTokens: 0}, "x", LoopHooks{}, nil)
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
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}}
	res, err := Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role != RoleTool {
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

// C-2：context 超限降级。首次 400(context_length) → 收紧历史 → 重试本步成功。
func TestRun_ContextLengthFallbackOnce(t *testing.T) {
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusOK},
		responses: []string{`{"error":{"message":"maximum context length exceeded"}}`, textResponse("recovered")},
	}
	llm := testClients(tr)
	cfg := LoopConfig{}
	res, err := Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
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
	llm := testClients(tr)
	cfg := LoopConfig{}
	_, err := Run(context.Background(), llm, cfg, "x", defaultHooks(cfg, nil), nil)
	if !errors.Is(err, ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength", err)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want 2 (only one fallback)", tr.calls)
	}
}

// §P1-A：工具输出超 limit 且 ToolOutputDir 启用时，全文落盘、历史 Content 含路径提示、文件内容==原文。
func TestRun_ToolOutputStoredOnTruncation(t *testing.T) {
	big := strings.Repeat("x", 5000) // > maxToolResultInHistory(4000)
	tool := Tool{Name: "bigtool", Call: func(context.Context, string) ToolResult { return ToolResult{Output: big} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "bigtool", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	dir := t.TempDir()
	cfg := LoopConfig{Tools: []Tool{tool}, ToolOutputDir: dir}
	res, err := Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	toolMsg := lastToolMessage(t, res.NewMessages)
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

// §P1-A：ToolOutputDir 空（默认禁用）→ 全链路无落盘 IO，行为等同 trimForHistory 硬截断（回归保护）。
func TestRun_ToolOutputDisabledByDefault(t *testing.T) {
	big := strings.Repeat("x", 5000)
	tool := Tool{Name: "bigtool", Call: func(context.Context, string) ToolResult { return ToolResult{Output: big} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "bigtool", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}}
	res, err := Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	toolMsg := lastToolMessage(t, res.NewMessages)
	if strings.Contains(toolMsg.Content, "已保存") {
		t.Errorf("禁用时不应含路径提示: %q", toolMsg.Content)
	}
	want := trimForHistory(big, 0, false)
	if toolMsg.Content != want {
		t.Errorf("禁用时应 == trimForHistory 硬截断: got len=%d want len=%d", len(toolMsg.Content), len(want))
	}
}

// §P1-A：SplitTruncate=true 的工具，落盘后预览仍含尾部关键行（头1/4+尾3/4 语义不被 store 改写）。
func TestRun_ToolOutputPreservesSplit(t *testing.T) {
	head := strings.Repeat("head-line\n", 700) // ~7000 字符头部
	tail := "FINAL_FAIL_LINE\n"
	big := head + tail
	tool := Tool{Name: "shelltool", SplitTruncate: true, Call: func(context.Context, string) ToolResult { return ToolResult{Output: big} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "shelltool", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	dir := t.TempDir()
	cfg := LoopConfig{Tools: []Tool{tool}, ToolOutputDir: dir}
	res, err := Run(context.Background(), llm, cfg, "q", defaultHooks(cfg, nil), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	toolMsg := lastToolMessage(t, res.NewMessages)
	if !strings.Contains(toolMsg.Content, "FINAL_FAIL_LINE") {
		t.Errorf("split 预览应保留尾部 FAIL 行: %q...(len %d)", toolMsg.Content[:min(60, len(toolMsg.Content))], len(toolMsg.Content))
	}
	if !strings.Contains(toolMsg.Content, "已保存") {
		t.Errorf("超限应触发落盘路径提示")
	}
}

// lastToolMessage 返回 msgs 中最后一条 role=tool 的消息（测试辅助）。
func lastToolMessage(t *testing.T, msgs []Message) Message {
	t.Helper()
	for idx := range slices.Backward(msgs) {
		if msgs[idx].Role == RoleTool {
			return msgs[idx]
		}
	}
	t.Fatalf("no tool message in msgs: %+v", msgs)
	return Message{}
}

// defaultHooks 复刻 main.go 的默认钩子组装（OnLLMError + OnBudget + ShapeToolResult），
// 供依赖原核心内置策略（失败重试/用量估算+预算熔断/工具结果截断+落盘）的集成测试一键恢复默认行为。
// 核心 Run 现零策略，测试须显式挂默认钩子才等价于旧行为。
func defaultHooks(cfg LoopConfig, logger *slog.Logger) LoopHooks {
	return LoopHooks{
		OnLLMError:      NewDefaultOnLLMError(logger),
		OnBudget:        NewDefaultOnBudget(cfg.MaxTotalTokens, logger),
		ShapeToolResult: NewDefaultShapeToolResult(cfg.Tools, cfg.ToolOutputDir, cfg.ToolOutputRetention, cfg.MaxToolResultChars, logger),
	}
}
