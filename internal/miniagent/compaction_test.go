package miniagent

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// testBudget 用 llm 构造 ContextBudget：Summarize 回调调 summarizeMiddle（maxChars=内置上限）。
// Model/CompactionModel/System/Tools 留零值（这些测试不关心 token 估算窗口，直接调 compactWithSummary）。
func testBudget(llm *ChatClient) ContextBudget {
	return ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys string, middle []Message) (string, Usage, error) {
			return summarizeMiddle(ctx, llm, model, sys, summaryMaxChars, middle)
		},
	}
}

// applyCompactionBarrier：有 summary → 返回最新 summary 及之后；无 → 原样。
func TestApplyCompactionBarrier(t *testing.T) {
	msgs := []Message{
		{Role: roleUser, Content: "old1"},
		{Role: roleUser, Kind: KindSummary, Content: "sum1"},
		{Role: roleUser, Content: "old2"},
		{Role: roleUser, Kind: KindSummary, Content: "sum2"},
		{Role: roleUser, Content: "recent"},
	}
	out := applyCompactionBarrier(msgs)
	if len(out) != 2 || out[0].Content != "sum2" || out[1].Content != "recent" {
		t.Errorf("barrier should keep latest summary onward: %+v", out)
	}
	none := applyCompactionBarrier([]Message{{Role: roleUser, Content: "x"}})
	if len(none) != 1 {
		t.Errorf("no summary → unchanged: %+v", none)
	}
}

// compactWithSummary：中段摘要为 KindSummary，结构（最早 1 轮 + summary + 最近 N 轮）正确，
// 且经 insertSummaryIntoNewMsgs 写入 newMsgs（持久化）。
func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("压缩摘要")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: roleUser, Content: "q" + strconv.Itoa(i)})
	}
	var newMsgs []Message
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if summary.Kind != KindSummary {
		t.Fatal("expected summary.Kind == KindSummary")
	}
	if err := validateToolPairing(out); err != nil {
		t.Errorf("result pairing broken: %v", err)
	}
	// 最早 1 轮 + summary + 最近 3 轮
	if len(out) != 1+1+3 {
		t.Errorf("out len = %d, want 5", len(out))
	}
	if out[1].Kind != KindSummary || !strings.Contains(out[1].Content, "压缩摘要") {
		t.Errorf("summary slot wrong: %+v", out[1])
	}
	insertSummaryIntoNewMsgs(&newMsgs, summary)
	if len(newMsgs) != 1 || newMsgs[0].Kind != KindSummary {
		t.Errorf("summary not persisted to newMsgs: %+v", newMsgs)
	}
}

// 中段配对断裂（孤立 tool 消息）→ 不摘要，返回 error（调用方回落有损）。
func TestCompactWithSummary_PairingBreakErrors(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []Message{
		{Role: roleUser, Content: "first"},
		{Role: roleTool, ToolCallID: "orphan", Content: "x"}, // 断裂
		{Role: roleUser, Content: "u2"},
		{Role: roleUser, Content: "u3"},
		{Role: roleUser, Content: "u4"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 1); err == nil {
		t.Fatal("expected pairing-break error")
	}
}

// 无中段可摘（轮数 ≤ 1+keepRecent）→ summary.Kind==""，不发 LLM 请求，msgs 原样。
func TestCompactWithSummary_NoMiddleNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("should-not-call")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []Message{{Role: roleUser, Content: "u1"}, {Role: roleUser, Content: "u2"}}
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 6)
	if err != nil || summary.Kind == KindSummary {
		t.Fatalf("expected (no-summary,nil), got (kind=%v,err=%v)", summary.Kind, err)
	}
	if len(out) != len(msgs) {
		t.Errorf("should be unchanged: out=%d", len(out))
	}
	if tr.calls != 0 {
		t.Errorf("should not call LLM without middle: calls=%d", tr.calls)
	}
}

// summarizeMiddle 的 LLM 错误上抛（不吞）。
func TestSummarizeMiddle_LLMError(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", summaryMaxChars, []Message{{Role: roleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}

// P2 摘要 token 入预算：summarizeMiddle 回传 LLM usage，供上游累加进 MaxTotalTokens 预算。
func TestSummarizeMiddle_ReturnsUsage(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"摘要"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":30}}`
	tr := &fakeTransport{responses: []string{body}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, usage, err := summarizeMiddle(context.Background(), llm, "m", "", summaryMaxChars, []Message{{Role: roleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if usage.InputTokens != 50 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want {50,30}", usage)
	}
}

// P3-1：摘要请求设置 MaxTokens=summaryMaxTokens。
func TestSummarizeMiddle_SetsMaxTokens(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", summaryMaxChars, []Message{{Role: roleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":1024`) {
		t.Errorf("摘要请求未设置 max_tokens=1024: %s", tr.lastBody)
	}
}

// compactWithSummary 应把 budget.CompactionModel 透传给 Summarize 回调。
func TestCompactWithSummary_CompactionModelOverride(t *testing.T) {
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	var gotModel string
	budget := ContextBudget{
		Model:           "main-model",
		CompactionModel: "compaction-model",
		Summarize: func(ctx context.Context, model, sys string, middle []Message) (string, Usage, error) {
			gotModel = model
			return summarizeMiddle(ctx, llm, model, sys, summaryMaxChars, middle)
		},
	}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: roleUser, Content: "q" + strconv.Itoa(i)})
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 3); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotModel != "compaction-model" {
		t.Errorf("Summarize model = %q, want compaction-model", gotModel)
	}
}
