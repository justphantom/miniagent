package miniagent

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

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
// 且 summary 写入 newMsgs（持久化）。
func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("压缩摘要")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: roleUser, Content: "q" + strconv.Itoa(i)})
	}
	var newMsgs []Message
	summarized, err := compactWithSummary(context.Background(), llm, "m", &msgs, 3, &newMsgs)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if !summarized {
		t.Fatal("expected summarized=true")
	}
	if err := validateToolPairing(msgs); err != nil {
		t.Errorf("result pairing broken: %v", err)
	}
	// 最早 1 轮 + summary + 最近 3 轮
	if len(msgs) != 1+1+3 {
		t.Errorf("msgs len = %d, want 5", len(msgs))
	}
	if msgs[1].Kind != KindSummary || !strings.Contains(msgs[1].Content, "压缩摘要") {
		t.Errorf("summary slot wrong: %+v", msgs[1])
	}
	if len(newMsgs) != 1 || newMsgs[0].Kind != KindSummary {
		t.Errorf("summary not persisted to newMsgs: %+v", newMsgs)
	}
}

// 中段配对断裂（孤立 tool 消息）→ 不摘要，返回 error（调用方回落有损）。
func TestCompactWithSummary_PairingBreakErrors(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	// 中段含孤立 tool（无前置 assistant tool_call）。
	msgs := []Message{
		{Role: roleUser, Content: "first"},
		{Role: roleTool, ToolCallID: "orphan", Content: "x"}, // 断裂
		{Role: roleUser, Content: "u2"},
		{Role: roleUser, Content: "u3"},
		{Role: roleUser, Content: "u4"},
	}
	var newMsgs []Message
	_, err := compactWithSummary(context.Background(), llm, "m", &msgs, 1, &newMsgs)
	if err == nil {
		t.Fatal("expected pairing-break error")
	}
}

// 无中段可摘（轮数 ≤ 1+keepRecent）→ (false, nil)，不发 LLM 请求。
func TestCompactWithSummary_NoMiddleNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("should-not-call")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []Message{{Role: roleUser, Content: "u1"}, {Role: roleUser, Content: "u2"}}
	before := len(msgs)
	var newMsgs []Message
	summarized, err := compactWithSummary(context.Background(), llm, "m", &msgs, 6, &newMsgs)
	if err != nil || summarized {
		t.Fatalf("expected (false,nil), got (%v,%v)", summarized, err)
	}
	if len(msgs) != before || len(newMsgs) != 0 {
		t.Errorf("should be unchanged: msgs=%d newMsgs=%d", len(msgs), len(newMsgs))
	}
	if tr.calls != 0 {
		t.Errorf("should not call LLM without middle: calls=%d", tr.calls)
	}
}

// summarizeMiddle 的 LLM 错误上抛（不吞）。
func TestSummarizeMiddle_LLMError(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, err := summarizeMiddle(context.Background(), llm, "m", []Message{{Role: roleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}
