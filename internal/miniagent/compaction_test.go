package miniagent

import (
	"context"
	"net/http"
	"path/filepath"
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
// 且 summary 写入 newMsgs（持久化）。注意此用例起点 newMsgs 为空（退化场景），实际 Run
// 场景 newMsgs 此时已含本轮 user_prompt，summary 须排其前——见 TestCompactWithSummary_SummaryBeforeUserPrompt。
func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("压缩摘要")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: roleUser, Content: "q" + strconv.Itoa(i)})
	}
	var newMsgs []Message
	summarized, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 3, &newMsgs)
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
	_, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 1, &newMsgs)
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
	summarized, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 6, &newMsgs)
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
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", []Message{{Role: roleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}

// P2 摘要 token 入预算：summarizeMiddle 回传 LLM usage，供上游累加进 MaxTotalTokens 预算。
func TestSummarizeMiddle_ReturnsUsage(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"摘要"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":30}}`
	tr := &fakeTransport{responses: []string{body}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, usage, err := summarizeMiddle(context.Background(), llm, "m", []Message{{Role: roleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if usage.InputTokens != 50 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want {50,30}", usage)
	}
}

// P3-1：摘要请求设置 MaxTokens=summaryMaxTokens，避免端点按默认上限输出后再被字符截断浪费 token。
func TestSummarizeMiddle_SetsMaxTokens(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", []Message{{Role: roleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":1024`) {
		t.Errorf("摘要请求未设置 max_tokens=1024: %s", tr.lastBody)
	}
}

// P1-1 回归：compactWithSummary 后 summary 必须排在 user_prompt 之前。loop.go Run 入口
// 先把本轮 user_prompt 加入 newMsgs，故此时 newMsgs=[user_prompt]；若 summary 尾 append
// 到 user_prompt 之后，下一轮 applyCompactionBarrier 会屏障掉本轮 user_prompt。
func TestCompactWithSummary_SummaryBeforeUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("历史摘要")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: roleUser, Content: "q" + strconv.Itoa(i)})
	}
	// 模拟 loop.go Run：入口已把本轮 user_prompt 加入 newMsgs 与 msgs。
	newMsgs := []Message{{Role: roleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)
	summarized, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 3, &newMsgs)
	if err != nil || !summarized {
		t.Fatalf("compactWithSummary: summarized=%v err=%v", summarized, err)
	}
	if len(newMsgs) != 2 {
		t.Fatalf("newMsgs len=%d, want 2 (summary+user_prompt): %+v", len(newMsgs), newMsgs)
	}
	if newMsgs[0].Kind != KindSummary {
		t.Errorf("newMsgs[0] 应为 summary，got %+v", newMsgs[0])
	}
	if newMsgs[1].Role != roleUser || newMsgs[1].Content != "本轮新问题" {
		t.Errorf("newMsgs[1] 应为本轮 user_prompt，got %+v", newMsgs[1])
	}
}

// P1-1 端到端：compactWithSummary 改写 newMsgs → AppendMessages 落盘 → LoadSession 读取
// → applyCompactionBarrier：本轮 user_prompt 必须仍在结果中。这是原 bug 漏测的根因——
// 测试只覆盖孤立 newMsgs（起点空），未走通 compact→persist→load→barrier 全链路。
func TestCompactWithSummary_CrossTurnBarrierPreservesUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("既往对话摘要")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	// 模拟上一轮 Run：历史 10 条 + 本轮 user_prompt。
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: roleUser, Content: "hist" + strconv.Itoa(i)})
	}
	newMsgs := []Message{{Role: roleUser, Content: "上一轮问题"}}
	msgs = append(msgs, newMsgs...)
	if _, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 3, &newMsgs); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	// 模拟上一轮 Run 末尾把 assistant 最终回答加入 newMsgs（接续对话依赖上一轮答案）。
	newMsgs = append(newMsgs, Message{Role: roleAssistant, Content: "上一轮回答"})

	// 落盘（main 仅成功轮调用 AppendMessages）。
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, newMsgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	// 下一轮 LoadSession + Run 入口的 applyCompactionBarrier。
	_, loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	barrier := applyCompactionBarrier(loaded)
	var hasPrompt, hasAnswer bool
	for _, m := range barrier {
		if m.Content == "上一轮问题" {
			hasPrompt = true
		}
		if m.Content == "上一轮回答" {
			hasAnswer = true
		}
	}
	if !hasPrompt {
		t.Errorf("P1-1 回归：barrier 后本轮 user_prompt 丢失：barrier=%+v", barrier)
	}
	if !hasAnswer {
		t.Errorf("barrier 后本轮 assistant 回答丢失：barrier=%+v", barrier)
	}
}

// P2 单轮多次压缩反转：单轮内 compactWithSummary 触发 ≥2 次时，第二次的中段含第一次写入的
// 旧 summary（已被进一步压进新 summary）。若前插前不剔旧 summary，newMsgs 变成
// [summary_new, summary_old, ...]，applyCompactionBarrier 反向找最后 summary 命中旧的 summary_old，
// 最新 summary_new 被屏障——压缩的「保最近轮」承诺失效。本用例验证：剔旧再前插后 newMsgs 只有
// 一个 KindSummary（最新）、排在最前，且 applyCompactionBarrier 命中它。
func TestCompactWithSummary_SingleTurnMultiplePreservesOrder(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要1"), textResponse("摘要2")}}
	llm := &HTTPClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	// 构造足够多轮的 msgs 使两次压缩都有中段。模拟 Run：入口已把本轮 user_prompt 加入 msgs/newMsgs。
	var msgs []Message
	for i := range 20 {
		msgs = append(msgs, Message{Role: roleUser, Content: "q" + strconv.Itoa(i)})
	}
	newMsgs := []Message{{Role: roleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)

	// 第一次摘要压缩。
	if summarized, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 3, &newMsgs); err != nil || !summarized {
		t.Fatalf("1st compactWithSummary: summarized=%v err=%v", summarized, err)
	}
	// 模拟步进：追加更多轮使再次超窗触发第二次压缩（Run 的 appendMsg 同时写 msgs/newMsgs）。
	for i := range 10 {
		m := Message{Role: roleUser, Content: "more" + strconv.Itoa(i)}
		msgs = append(msgs, m)
		newMsgs = append(newMsgs, m)
	}
	if summarized, _, err := compactWithSummary(context.Background(), llm, "m", &msgs, 3, &newMsgs); err != nil || !summarized {
		t.Fatalf("2nd compactWithSummary: summarized=%v err=%v", summarized, err)
	}

	// newMsgs 应只剩一条 KindSummary（最新），且排在最前。
	count := 0
	for _, m := range newMsgs {
		if m.Kind == KindSummary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 summary after two compactions (old must be dropped), got %d: %+v", count, newMsgs)
	}
	if newMsgs[0].Kind != KindSummary || !strings.Contains(newMsgs[0].Content, "摘要2") {
		t.Errorf("newest summary must be first: %+v", newMsgs[0])
	}
	// applyCompactionBarrier 必须命中最新摘要（无旧 summary 残留导致反向命中错位）。
	barrier := applyCompactionBarrier(newMsgs)
	if len(barrier) == 0 || barrier[0].Kind != KindSummary || !strings.Contains(barrier[0].Content, "摘要2") {
		t.Errorf("barrier should start at newest summary: %+v", barrier)
	}
}
