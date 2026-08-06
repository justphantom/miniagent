package miniagent

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// P1-1 回归：compactWithSummary 后 summary 必须排在 user_prompt 之前。loop.go Run 入口
// 先把本轮 user_prompt 加入 newMsgs，故此时 newMsgs=[user_prompt]；insertSummaryIntoNewMsgs
// 前插 summary，使其排在 user_prompt 之前——否则下一轮 applyCompactionBarrier 会屏障掉本轮 user_prompt。
func TestCompactWithSummary_SummaryBeforeUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("历史摘要")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	// 模拟 loop.go Run：入口已把本轮 user_prompt 加入 newMsgs 与 msgs。
	newMsgs := []Message{{Role: RoleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)
	_, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary.Kind != KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	insertSummaryIntoNewMsgs(&newMsgs, summary)
	if len(newMsgs) != 2 {
		t.Fatalf("newMsgs len=%d, want 2 (summary+user_prompt): %+v", len(newMsgs), newMsgs)
	}
	if newMsgs[0].Kind != KindSummary {
		t.Errorf("newMsgs[0] 应为 summary，got %+v", newMsgs[0])
	}
	if newMsgs[1].Role != RoleUser || newMsgs[1].Content != "本轮新问题" {
		t.Errorf("newMsgs[1] 应为本轮 user_prompt，got %+v", newMsgs[1])
	}
}

// P1-1 端到端：compactWithSummary + insertSummaryIntoNewMsgs → AppendMessages 落盘 → LoadSession 读取
// → applyCompactionBarrier：本轮 user_prompt 必须仍在结果中。
func TestCompactWithSummary_CrossTurnBarrierPreservesUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("既往对话摘要")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 10 {
		msgs = append(msgs, Message{Role: RoleUser, Content: "hist" + strconv.Itoa(i)})
	}
	newMsgs := []Message{{Role: RoleUser, Content: "上一轮问题"}}
	msgs = append(msgs, newMsgs...)
	_, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	insertSummaryIntoNewMsgs(&newMsgs, summary)
	// 模拟上一轮 Run 末尾把 assistant 最终回答加入 newMsgs（接续对话依赖上一轮答案）。
	newMsgs = append(newMsgs, Message{Role: RoleAssistant, Content: "上一轮回答"})

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, newMsgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
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

// P2 单轮多次压缩反转：单轮内压缩触发 ≥2 次时，第二次的中段含第一次写入的旧 summary（已被进一步
// 压进新 summary）。insertSummaryIntoNewMsgs 剔旧再前插后 newMsgs 只有一个 KindSummary（最新）、
// 排在最前，且 applyCompactionBarrier 命中它。
func TestCompactWithSummary_SingleTurnMultiplePreservesOrder(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要1"), textResponse("摘要2")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []Message
	for i := range 20 {
		msgs = append(msgs, Message{Role: RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	newMsgs := []Message{{Role: RoleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)

	out, summary1, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary1.Kind != KindSummary {
		t.Fatalf("1st compactWithSummary: kind=%v err=%v", summary1.Kind, err)
	}
	msgs = out
	insertSummaryIntoNewMsgs(&newMsgs, summary1)
	// 模拟步进：追加更多轮使再次超窗触发第二次压缩（Run 的 appendMsg 同时写 msgs/newMsgs）。
	for i := range 10 {
		m := Message{Role: RoleUser, Content: "more" + strconv.Itoa(i)}
		msgs = append(msgs, m)
		newMsgs = append(newMsgs, m)
	}
	out2, summary2, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary2.Kind != KindSummary {
		t.Fatalf("2nd compactWithSummary: kind=%v err=%v", summary2.Kind, err)
	}
	_ = out2
	insertSummaryIntoNewMsgs(&newMsgs, summary2)

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
	barrier := applyCompactionBarrier(newMsgs)
	if len(barrier) == 0 || barrier[0].Kind != KindSummary || !strings.Contains(barrier[0].Content, "摘要2") {
		t.Errorf("barrier should start at newest summary: %+v", barrier)
	}
}

// P2-1 跨轮继承：上轮 LoadSession 带入的旧 KindSummary 经 applyCompactionBarrier 落在 msgs 头，
// splitRounds 使其单独成 rounds[0]；compactWithSummary 检测到后（§P0-A 默认路径）抽作
// previousSummary 经 UPDATE 模式下传，旧摘要文本仍出现在 LLM 请求体（system 的
// <previous-summary> 块）中，真正继承而非断链。
func TestCompactWithSummary_CrossTurnInheritsLegacySummary(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("新摘要内容")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []Message{
		{Role: RoleUser, Kind: KindSummary, Content: "[既往对话摘要]\n远古摘要内容-旧"},
		{Role: RoleUser, Content: "real0"},
		{Role: RoleUser, Content: "real1"},
		{Role: RoleUser, Content: "real2"},
		{Role: RoleUser, Content: "real3"},
		{Role: RoleUser, Content: "real4"},
		{Role: RoleUser, Content: "real5"},
	}
	newMsgs := []Message{{Role: RoleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)

	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary.Kind != KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	_ = newMsgs

	// (a) LLM 输入必须含旧 summary 文本——修复前 middle 排除 rounds[0]，继承断链。
	if len(tr.bodies) == 0 {
		t.Fatal("expected summarizeMiddle to be called")
	}
	if !strings.Contains(tr.bodies[0], "远古摘要内容-旧") {
		t.Errorf("LLM 输入应含旧 summary 文本以真正继承：body=%s", tr.bodies[0])
	}
	// (b) 结果恰 1 条 KindSummary，位于头部（旧 summary 已并入新 summary，不独立保留）。
	count := 0
	for _, m := range out {
		if m.Kind == KindSummary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 KindSummary in out, got %d: %+v", count, out)
	}
	if len(out) == 0 || out[0].Kind != KindSummary || !strings.Contains(out[0].Content, "新摘要内容") {
		t.Errorf("head should be the new summary: %+v", out)
	}
	if len(out) != 1+3 {
		t.Errorf("out len = %d, want 4 (1 summary + 3 recent): %+v", len(out), out)
	}
	// (c) applyCompactionBarrier 命中头部新 summary。
	barrier := applyCompactionBarrier(out)
	if len(barrier) == 0 || barrier[0].Kind != KindSummary || !strings.Contains(barrier[0].Content, "新摘要内容") {
		t.Errorf("barrier should start at new summary: %+v", barrier)
	}
}

// FitHistory：未超 window（ContextWindow<=0）→ 原样 noop。
func TestFitHistory_NoWindowNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []Message{{Role: RoleUser, Content: "q"}}
	out, summary, summarized, usage, err := FitHistory(context.Background(), msgs, ContextBudget{Summarize: testBudget(llm).Summarize}, nil)
	if err != nil || summarized || summary.Kind == KindSummary {
		t.Fatalf("expected noop, got out=%d summarized=%v kind=%v err=%v", len(out), summarized, summary.Kind, err)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("noop usage should be zero: %+v", usage)
	}
	if tr.calls != 0 {
		t.Errorf("should not call LLM: calls=%d", tr.calls)
	}
}

// FitHistory：摘要失败回落有损 compactHistory，最终不超窗→不报错、summarized=false。
func TestFitHistory_SummarizeErrorFallsBackLossy(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	big := strings.Repeat("x", 1000) // 每条 ~250 tokens，使 30 条远超窗、压到 4 条后落回窗内
	var msgs []Message
	for range 30 {
		msgs = append(msgs, Message{Role: RoleUser, Content: big})
	}
	// ContextWindow=4000 → 阈值 3200：30 条(~7900)超窗触发；compactHistory 压到 4 条(~1400)落回窗内。
	budget := ContextBudget{ContextWindow: 4000, KeepRecent: 3, Summarize: testBudget(llm).Summarize}
	out, _, summarized, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("lossy fallback should not error when it fits: %v", err)
	}
	if summarized {
		t.Error("summarize failed → summarized should be false")
	}
	if len(out) >= len(msgs) {
		t.Errorf("lossy compaction should shrink msgs: out=%d in=%d", len(out), len(msgs))
	}
}
