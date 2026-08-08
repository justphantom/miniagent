package compaction

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/session"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// P1-1 回归：compactWithSummary 后 summary 必须排在 user_prompt 之前。loop.go miniagent.Run 入口
// 先把本轮 user_prompt 加入 newMsgs，故此时 newMsgs=[user_prompt]；合并路径（生产为 mergePersisted）
// 前插 summary，使其排在 user_prompt 之前——否则下一轮 applyCompactionBarrier 会屏障掉本轮 user_prompt。
func TestCompactWithSummary_SummaryBeforeUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("历史摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	// 模拟 loop.go miniagent.Run：入口已把本轮 user_prompt 加入 newMsgs 与 msgs。
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)
	_, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	if len(newMsgs) != 2 {
		t.Fatalf("newMsgs len=%d, want 2 (summary+user_prompt): %+v", len(newMsgs), newMsgs)
	}
	if newMsgs[0].Kind != miniagent.KindSummary {
		t.Errorf("newMsgs[0] 应为 summary，got %+v", newMsgs[0])
	}
	if newMsgs[1].Role != miniagent.RoleUser || newMsgs[1].Content != "本轮新问题" {
		t.Errorf("newMsgs[1] 应为本轮 user_prompt，got %+v", newMsgs[1])
	}
}

// P1-1 端到端：compactWithSummary + summary 前插合并（生产为 mergePersisted）→ session.AppendMessages 落盘
// → session.LoadSession 读取 → applyCompactionBarrier：本轮 user_prompt 必须仍在结果中。
func TestCompactWithSummary_CrossTurnBarrierPreservesUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("既往对话摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "hist" + strconv.Itoa(i)})
	}
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "上一轮问题"}}
	msgs = append(msgs, newMsgs...)
	_, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	// 模拟上一轮 miniagent.Run 末尾把 assistant 最终回答加入 newMsgs（接续对话依赖上一轮答案）。
	newMsgs = append(newMsgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "上一轮回答"})

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := session.AppendMessages(path, session.SessionMeta{ID: "s"}, newMsgs); err != nil {
		t.Fatalf("session.AppendMessages: %v", err)
	}
	_, loaded, err := session.LoadSession(path)
	if err != nil {
		t.Fatalf("session.LoadSession: %v", err)
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

// mergeSummaryIntoNewMsgs 与 loop.go mergePersisted 等价的测试侧合并：剔 newMsgs 中已有
// KindSummary 后把 summary 前插（跨轮 barrier 命中最新 summary 的语义）。
func mergeSummaryIntoNewMsgs(newMsgs *[]miniagent.Message, summary miniagent.Message) {
	filtered := make([]miniagent.Message, 0, len(*newMsgs)+1)
	for _, m := range *newMsgs {
		if m.Kind != miniagent.KindSummary {
			filtered = append(filtered, m)
		}
	}
	*newMsgs = append([]miniagent.Message{summary}, filtered...)
}

// P2 单轮多次压缩反转：单轮内压缩触发 ≥2 次时，第二次的中段含第一次写入的旧 summary（已被进一步
// 压进新 summary）。合并路径（生产为 mergePersisted，按 Kind 剔旧再前插）后 newMsgs 只有一个
// miniagent.KindSummary（最新）、排在最前，且 applyCompactionBarrier 命中它。
func TestCompactWithSummary_SingleTurnMultiplePreservesOrder(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要1"), textResponse("摘要2")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)

	out, summary1, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary1.Kind != miniagent.KindSummary {
		t.Fatalf("1st compactWithSummary: kind=%v err=%v", summary1.Kind, err)
	}
	msgs = out
	mergeSummaryIntoNewMsgs(&newMsgs, summary1)
	// 模拟步进：追加更多轮使再次超窗触发第二次压缩（miniagent.Run 的 appendMsg 同时写 msgs/newMsgs）。
	for i := range 10 {
		m := miniagent.Message{Role: miniagent.RoleUser, Content: "more" + strconv.Itoa(i)}
		msgs = append(msgs, m)
		newMsgs = append(newMsgs, m)
	}
	out2, summary2, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary2.Kind != miniagent.KindSummary {
		t.Fatalf("2nd compactWithSummary: kind=%v err=%v", summary2.Kind, err)
	}
	_ = out2
	mergeSummaryIntoNewMsgs(&newMsgs, summary2)

	count := 0
	for _, m := range newMsgs {
		if m.Kind == miniagent.KindSummary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 summary after two compactions (old must be dropped), got %d: %+v", count, newMsgs)
	}
	if newMsgs[0].Kind != miniagent.KindSummary || !strings.Contains(newMsgs[0].Content, "摘要2") {
		t.Errorf("newest summary must be first: %+v", newMsgs[0])
	}
	barrier := applyCompactionBarrier(newMsgs)
	if len(barrier) == 0 || barrier[0].Kind != miniagent.KindSummary || !strings.Contains(barrier[0].Content, "摘要2") {
		t.Errorf("barrier should start at newest summary: %+v", barrier)
	}
}

// P2-1 跨轮继承：上轮 session.LoadSession 带入的旧 miniagent.KindSummary 经 applyCompactionBarrier 落在 msgs 头，
// splitRounds 使其单独成 rounds[0]；compactWithSummary 检测到后（§P0-A 默认路径）抽作
// previousSummary 经 UPDATE 模式下传，旧摘要文本仍出现在 LLM 请求体（system 的
// <previous-summary> 块）中，真正继承而非断链。
func TestCompactWithSummary_CrossTurnInheritsLegacySummary(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("新摘要内容")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "[既往对话摘要]\n远古摘要内容-旧"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "real5"},
	}
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "本轮新问题"}}
	msgs = append(msgs, newMsgs...)

	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
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
	// (b) 结果恰 1 条 miniagent.KindSummary，位于头部（旧 summary 已并入新 summary，不独立保留）。
	count := 0
	for _, m := range out {
		if m.Kind == miniagent.KindSummary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 miniagent.KindSummary in out, got %d: %+v", count, out)
	}
	if len(out) == 0 || out[0].Kind != miniagent.KindSummary || !strings.Contains(out[0].Content, "新摘要内容") {
		t.Errorf("head should be the new summary: %+v", out)
	}
	if len(out) != 1+3 {
		t.Errorf("out len = %d, want 4 (1 summary + 3 recent): %+v", len(out), out)
	}
	// (c) applyCompactionBarrier 命中头部新 summary。
	barrier := applyCompactionBarrier(out)
	if len(barrier) == 0 || barrier[0].Kind != miniagent.KindSummary || !strings.Contains(barrier[0].Content, "新摘要内容") {
		t.Errorf("barrier should start at new summary: %+v", barrier)
	}
}

// FitHistory：未超 window（ContextWindow<=0）→ 原样 noop。
func TestFitHistory_NoWindowNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}
	out, summary, summarized, _, usage, err := FitHistory(context.Background(), msgs, ContextBudget{Summarize: testBudget(llm).Summarize}, nil)
	if err != nil || summarized || summary.Kind == miniagent.KindSummary {
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
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	big := strings.Repeat("x", 1000) // 每条 ~250 tokens，使 30 条远超窗、压到 4 条后落回窗内
	var msgs []miniagent.Message
	for range 30 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: big})
	}
	// ContextWindow=4000 → 阈值 3200：30 条(~7900)超窗触发；compactHistory 压到 4 条(~1400)落回窗内。
	budget := ContextBudget{ContextWindow: 4000, KeepRecent: 3, Summarize: testBudget(llm).Summarize}
	out, _, summarized, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
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

// L3-6：FitHistory 是导出函数，直接调用方若漏设 Summarize（Force=true 或超窗进 compactWithSummary）→
// nil 守卫返回 error → FitHistory 回落有损 compactHistory（不 panic、summarized=false、不报错）。
func TestFitHistory_NilSummarizeFallsBackLossy(t *testing.T) {
	big := strings.Repeat("x", 1000) // 每条 ~250 tokens
	var msgs []miniagent.Message
	for range 30 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: big})
	}
	// Force=true 跳过 4/5 门控直接进 compactWithSummary；Summarize=nil（零值）触发 nil 守卫→有损 fallback。
	budget := ContextBudget{ContextWindow: 4000, KeepRecent: 3, Force: true}
	out, _, summarized, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("nil Summarize 应回落有损而非报错: %v", err)
	}
	if summarized {
		t.Error("Summarize=nil → summarized 应为 false（有损 fallback）")
	}
	if len(out) >= len(msgs) {
		t.Errorf("有损 fallback 应收缩: out=%d in=%d", len(out), len(msgs))
	}
}
