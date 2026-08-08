package compaction

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent/policy"
	"github.com/justphantom/miniagent/internal/miniagent/session"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// testBudget 用 llm 构造 ContextBudget：Summarize 回调调 summarizeMiddle（maxChars=内置上限）。
// Model/CompactionModel/System/Tools 留零值（这些测试不关心 token 估算窗口，直接调 compactWithSummary）。
func testBudget(llm *openai.ChatClient) ContextBudget {
	return ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, "", "", "", summaryMaxChars, 0, middle)
		},
	}
}

// jointTailBudget：CW<=0 回落 preserveRecentTokens(=0)；否则 min(CW×4/5 − reqOverhead − headAdj − summaryEstimate, userCap)。
// headAdj 在默认路径下旧 summary 不进 out → 0。reqOverhead=EstimateTokens(nil,"",nil)=SystemOverhead=400；
// head="q" 非summary → estimateRoundTokens=4；summaryEstimate=summaryMaxChars/2+Envelope=2504。
func TestJointTailBudget(t *testing.T) {
	head := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}                                   // 首轮非 summary
	headSum := []miniagent.Message{{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "旧摘要"}} // UPDATE 路径 head
	mk := func(cw int) ContextBudget { return ContextBudget{ContextWindow: cw, System: "", Tools: nil} }
	cases := []struct {
		name string
		bud  ContextBudget
		head []miniagent.Message
		want int
	}{
		{"CW<=0 无窗口回落 0", mk(0), head, 0},                      // preserveRecentTokens(CW<=0)=0
		{"大CW 128k 取 userCap 8000", mk(128000), head, 8000},    // avail 99492 > cap 8000
		{"大CW 128k UPDATE head 不扣", mk(128000), headSum, 8000}, // headAdj=0 但 cap 主导
		{"中CW 5120 扣 head", mk(5120), head, 1188},              // 4096-400-4-2504=1188 < cap 2000
		{"中CW 5120 UPDATE 不扣 head", mk(5120), headSum, 1192},   // 4096-400-0-2504=1192
		{"小CW 2048 avail<=0 归零", mk(2048), head, 0},            // 1638-400-4-2504<0 → 0
	}
	for _, c := range cases {
		if got := jointTailBudget(c.bud, c.head); got != c.want {
			t.Errorf("%s: jointTailBudget=%d, want %d", c.name, got, c.want)
		}
	}
}

// FitHistory 联合预算（§B）：CW=5120 + summaryMaxChars=5000 的中等窗口，当前（独立 tail 预算）会
// head+summary+tail 超窗 trim 后仍超 → 终止 error；联合预算让 tail 让步 → out est< CW×4/5，err==nil。
// 20 轮 × 600 中文字（≈6480 token > 门控 4096 触发摘要），假摘要回调返回满 5000 字。
func TestFitHistory_JointBudgetSavesMidWindow(t *testing.T) {
	bigSummary := strings.Repeat("摘", 5000)
	budget := ContextBudget{
		ContextWindow:   5120,
		SummaryMaxChars: 5000,
		Model:           "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return bigSummary, miniagent.Usage{}, nil
		},
	}
	var msgs []miniagent.Message
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	out, _, summarized, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("CW=5120 联合预算下应不终止，err=%v（out=%d msgs，summarized=%v）", err, len(out), summarized)
	}
	if !summarized {
		t.Fatal("期望触发摘要压缩")
	}
}

// FitHistory 压缩：tail 保留原文 reasoning（改 Commit 语义后压缩分支不 strip out），committed=true。
// 改前 applyContextStrips(out) 清非最近 1 条 reasoning，tail 多条 assistant 只剩最新 1 条；改后全保留。
func TestFitHistory_PreservesTailReasoningOnCompaction(t *testing.T) {
	bigSummary := strings.Repeat("摘", 5000)
	budget := ContextBudget{
		ContextWindow:   5120,
		SummaryMaxChars: 5000,
		Model:           "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return bigSummary, miniagent.Usage{}, nil
		},
	}
	var msgs []miniagent.Message
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "head"})
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R1", Reasoning: "思考1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R2", Reasoning: "思考2"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q2"})
	out, _, _, committed, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	reasoningCnt := 0
	for _, m := range out {
		if m.Role == miniagent.RoleAssistant && m.Reasoning != "" {
			reasoningCnt++
		}
	}
	if reasoningCnt < 2 {
		t.Errorf("压缩 tail 应保留 ≥2 条原文 reasoning（改前 strip 清非最近 1 条），实际 %d", reasoningCnt)
	}
	if !committed {
		t.Error("压缩应 committed=true")
	}
}

// FitHistory 非压缩：committed=false（strip 仅本轮 View，transcript 保留原文不替换）。
func TestFitHistory_NonCompactNotCommitted(t *testing.T) {
	budget := ContextBudget{
		ContextWindow: 128000,
		Model:         "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return "s", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q1"}, {Role: miniagent.RoleUser, Content: "q2"}}
	_, _, _, committed, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if committed {
		t.Error("非压缩应 committed=false（strip 仅本轮 View，不替换 transcript）")
	}
}

// deriveSummaryMaxChars（方向 A）：configured>0 覆盖；cw<=0 回落 5000；否则 min(5000, cw/5)。
func TestDeriveSummaryMaxChars(t *testing.T) {
	cases := []struct {
		cw, configured, want int
	}{
		{4096, 0, 819},     // 小窗口缩放
		{2048, 0, 409},     // 更小窗口
		{24999, 0, 4999},   // 刚 below 内置上限
		{25000, 0, 5000},   // 边界 cw/5=5000 不<5000 → 内置
		{40000, 0, 5000},   // 大窗口 clamp 内置
		{0, 0, 5000},       // 无窗口回落内置
		{4096, 3000, 3000}, // 用户显式覆盖
	}
	for _, c := range cases {
		if got := deriveSummaryMaxChars(c.cw, c.configured); got != c.want {
			t.Errorf("deriveSummaryMaxChars(%d, %d) = %d, want %d", c.cw, c.configured, got, c.want)
		}
	}
}

// NewCompaction：不设 SummaryMaxChars 时，maxChars 随 ContextWindow 缩放派生（方向 A），maxSummaryTokens 自动联动。
// CW=4096 → maxChars=819 → maxSummaryTokens=819/2=409。20 轮 × 600 中文字触发摘要；忽略 before 可能的终止 error，
// 只验摘要请求携带的派生 max_tokens（A+B 下 CW=4096 实测不终止，但断言不依赖此）。
func TestNewCompaction_ScalesSummaryMaxCharsByWindow(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	before, _ := NewCompaction(CompactionOptions{
		Chat:          llm,
		ContextWindow: 4096, // 不设 SummaryMaxChars → 派生 819 → maxSummaryTokens 409
	})
	var msgs []miniagent.Message
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	_, _ = before(context.Background(), miniagent.StepInput{Step: 1, Msgs: msgs})
	if tr.calls == 0 {
		t.Fatal("期望触发摘要 LLM 调用")
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":409`) {
		t.Errorf("期望 max_tokens=819/2=409（summaryMaxChars 随 CW 缩放派生），实际: %s", tr.lastBody)
	}
}

// compactWithSummary 摘要前对 middle 全压 strip：捕获 Summarize 收到的 middle，断言 reasoning 已清 + read 已 dedup + 配对完整。
func TestCompactWithSummary_StripsMiddleBeforeSummarize(t *testing.T) {
	var captured []miniagent.Message
	budget := ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			captured = middle
			return "摘要", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "head 首轮"}}
	for i := range 6 {
		id := "c" + strconv.Itoa(i)
		msgs = append(msgs, miniagent.Message{
			Role:      miniagent.RoleAssistant,
			Content:   "看文件",
			Reasoning: strings.Repeat("思", 800),
			ToolCalls: []miniagent.ToolCall{{ID: id, Name: "read", Args: `{"path":"/f.go","offset":1}`}},
		})
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleTool, ToolCallID: id, Content: strings.Repeat("x", 800)})
	}
	for range 4 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "recent"})
	}
	out, _, _, err := compactWithSummary(context.Background(), budget, msgs, 4)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	for _, m := range captured {
		if m.Role == miniagent.RoleAssistant && m.Reasoning != "" {
			t.Errorf("middle 进摘要前 reasoning 应被全压清，仍存 %d 字", len([]rune(m.Reasoning)))
		}
	}
	// dedup（P6）现生效（windowStartOf(0)=len 全窗口外）：6 条同 path/offset read 留时间序最后，前几个占位。
	deduped := 0
	for _, m := range captured {
		if m.Role == miniagent.RoleTool && strings.Contains(m.Content, "已被更新的读取取代") {
			deduped++
		}
	}
	if deduped == 0 {
		t.Errorf("期望同 path/offset read 被 dedup 占位，实际 captured tool 无占位")
	}
	// reasoning 清（~2400 token）+ read dedup（6→1）后体积应 < 1500。
	capturedTokens := policy.EstimateTokens(captured, "", nil)
	if capturedTokens > 1500 {
		t.Errorf("middle strip 后摘要体积应 < 1500（reasoning 清 + read dedup），实际 %d", capturedTokens)
	}
	if err := session.ValidateToolPairing(out); err != nil {
		t.Errorf("strip 后配对断裂: %v", err)
	}
}

// windowStartOf：keepN<=0 → len(msgs)（全窗口外=全压）；keepN 条 assistant 存在 → 第 keepN 条（从后）index；不足 → 0。
func TestWindowStartOf(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser},
		{Role: miniagent.RoleAssistant},
		{Role: miniagent.RoleUser},
		{Role: miniagent.RoleAssistant},
		{Role: miniagent.RoleUser},
	} // assistant at 1,3；len=5
	cases := []struct{ keepN, want int }{
		{0, 5},  // 全窗口外（全压）
		{-1, 5}, // 同上
		{1, 3},  // 最近 1 条 assistant=index3
		{2, 1},  // 最近 2 条=index1
		{3, 0},  // 不足（仅 2 assistant）→ 0（全窗口内=全保留）
	}
	for _, c := range cases {
		if got := windowStartOf(msgs, c.keepN); got != c.want {
			t.Errorf("windowStartOf(keepN=%d)=%d, want %d", c.keepN, got, c.want)
		}
	}
}

// applyCompactionBarrier：有 summary → 返回最新 summary 及之后；无 → 原样。
func TestApplyCompactionBarrier(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "old1"},
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "sum1"},
		{Role: miniagent.RoleUser, Content: "old2"},
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "sum2"},
		{Role: miniagent.RoleUser, Content: "recent"},
	}
	out := applyCompactionBarrier(msgs)
	if len(out) != 2 || out[0].Content != "sum2" || out[1].Content != "recent" {
		t.Errorf("barrier should keep latest summary onward: %+v", out)
	}
	none := applyCompactionBarrier([]miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}})
	if len(none) != 1 {
		t.Errorf("no summary → unchanged: %+v", none)
	}
}

// compactWithSummary：中段摘要为 miniagent.KindSummary，结构（最早 1 轮 + summary + 最近 N 轮）正确，
// 且 summary 可并入 newMsgs（持久化语义；生产路径为 loop.go mergePersisted）。
func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("压缩摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	var newMsgs []miniagent.Message
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if summary.Kind != miniagent.KindSummary {
		t.Fatal("expected summary.Kind == miniagent.KindSummary")
	}
	if err := session.ValidateToolPairing(out); err != nil {
		t.Errorf("result pairing broken: %v", err)
	}
	// 最早 1 轮 + summary + 最近 3 轮
	if len(out) != 1+1+3 {
		t.Errorf("out len = %d, want 5", len(out))
	}
	if out[1].Kind != miniagent.KindSummary || !strings.Contains(out[1].Content, "压缩摘要") {
		t.Errorf("summary slot wrong: %+v", out[1])
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	if len(newMsgs) != 1 || newMsgs[0].Kind != miniagent.KindSummary {
		t.Errorf("summary not persisted to newMsgs: %+v", newMsgs)
	}
}

// 中段配对断裂（孤立 tool 消息）→ 不摘要，返回 error（调用方回落有损）。
func TestCompactWithSummary_PairingBreakErrors(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleTool, ToolCallID: "orphan", Content: "x"}, // 断裂
		{Role: miniagent.RoleUser, Content: "u2"},
		{Role: miniagent.RoleUser, Content: "u3"},
		{Role: miniagent.RoleUser, Content: "u4"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 1); err == nil {
		t.Fatal("expected pairing-break error")
	}
}

// 无中段可摘（轮数 ≤ 1+keepRecent）→ summary.Kind==""，不发 LLM 请求，msgs 原样。
func TestCompactWithSummary_NoMiddleNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("should-not-call")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "u1"}, {Role: miniagent.RoleUser, Content: "u2"}}
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 6)
	if err != nil || summary.Kind == miniagent.KindSummary {
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
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}

// P2 摘要 token 入预算：summarizeMiddle 回传 LLM usage，供上游累加进 MaxTotalTokens 预算。
func TestSummarizeMiddle_ReturnsUsage(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"摘要"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":30}}`
	tr := &fakeTransport{responses: []string{body}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, usage, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if usage.InputTokens != 50 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want {50,30}", usage)
	}
}

// P3-1：摘要请求设置 MaxTokens=summaryMaxTokens（默认从 summaryMaxChars 派生 = summaryMaxChars/2）。
func TestSummarizeMiddle_SetsMaxTokens(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// 引用常量而非魔法数：summaryMaxTokens 现派生自 summaryMaxChars/2，未来 chars 变化自动跟随。
	if !strings.Contains(tr.lastBody, `"max_tokens":`+strconv.Itoa(summaryMaxTokens)) {
		t.Errorf("摘要请求未设置 max_tokens=%d: %s", summaryMaxTokens, tr.lastBody)
	}
}

// deriveSummaryMaxTokens：configured>0 覆盖；否则从 maxChars 派生（chars/2，CJK 最密口径）；maxChars<2 回落兜底常量。
func TestDeriveSummaryMaxTokens(t *testing.T) {
	cases := []struct {
		maxChars, configured, want int
	}{
		{5000, 0, 2500},          // 默认派生（summaryMaxChars/2）
		{5000, 512, 512},         // 用户显式覆盖
		{8000, 0, 4000},          // 只配 chars → token 跟随
		{0, 0, summaryMaxTokens}, // maxChars<=0 防御兜底
		{1, 0, summaryMaxTokens}, // maxChars<2 兜底
	}
	for _, c := range cases {
		if got := deriveSummaryMaxTokens(c.maxChars, c.configured); got != c.want {
			t.Errorf("deriveSummaryMaxTokens(%d, %d) = %d, want %d", c.maxChars, c.configured, got, c.want)
		}
	}
}

// NewCompaction：只配 SummaryMaxChars（不配 token）→ 摘要请求 max_tokens 由 chars 派生（chars/2）。
// 验证「配 chars → token 自动跟随」端到端契约。CW=1 使历史必超 4/5 门控触发摘要；压缩后仍 >0 token
// 会命中 FitHistory 终止保护（返回 error）——但摘要 LLM 调用已先于终止发生，tr.lastBody 已记录，故忽略该 error。
func TestNewCompaction_DerivesMaxTokensFromChars(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	before, after := NewCompaction(CompactionOptions{
		Chat:            llm,
		ContextWindow:   1,
		SummaryMaxChars: 8000, // 不设 SummaryMaxTokens → 应派生 8000/2=4000
	})
	if after != nil {
		t.Fatalf("after 应为 nil（溢出检测已并入 before），实际非 nil")
	}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	// CW=1 触发摘要后命中终止保护：忽略 error，靠 tr.calls/lastBody 验证派生。
	_, _ = before(context.Background(), miniagent.StepInput{Step: 1, Msgs: msgs})
	if tr.calls == 0 {
		t.Fatal("期望触发摘要 LLM 调用")
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":4000`) {
		t.Errorf("期望 max_tokens=8000/2=4000（chars 派生），实际: %s", tr.lastBody)
	}
}

// compactWithSummary 应把 budget.CompactionModel 透传给 Summarize 回调。
func TestCompactWithSummary_CompactionModelOverride(t *testing.T) {
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	var gotModel string
	budget := ContextBudget{
		Model:           "main-model",
		CompactionModel: "compaction-model",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotModel = model
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, "", "", "", summaryMaxChars, 0, middle)
		},
	}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 3); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotModel != "compaction-model" {
		t.Errorf("Summarize model = %q, want compaction-model", gotModel)
	}
}

// §P0-A：buildSummarizerSystem 三模式。CREATE：含模板 6 段、不含 <previous-summary>。
func TestBuildSummarizerSystem_CreateMode(t *testing.T) {
	got := buildSummarizerSystem("", "", "", "", "", 5000)
	for _, want := range []string{"## 目标", "## 关键细节", "## 进展状态", "## 下一步", "## 相关文件"} {
		if !strings.Contains(got, want) {
			t.Errorf("CREATE 模式应含模板段 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "<previous-summary>") {
		t.Errorf("CREATE 模式不应含 <previous-summary>：%s", got)
	}
}

// §P0-A：UPDATE：含 <previous-summary> 块包裹旧摘要 + update 指令 + 模板 6 段。
func TestBuildSummarizerSystem_UpdateMode(t *testing.T) {
	got := buildSummarizerSystem("", "旧锚点", "", "", "", 5000)
	for _, want := range []string{"<previous-summary>\n旧锚点\n</previous-summary>", "更新已有的锚定摘要", "## 目标", "## 相关文件"} {
		if !strings.Contains(got, want) {
			t.Errorf("UPDATE 模式应含 %q：\n%s", want, got)
		}
	}
}

// §P0-A：override：summarizerPrompt 非空 → 全量接管，{max_chars} 占位符替换，不含模板/previous-summary。
func TestBuildSummarizerSystem_Override(t *testing.T) {
	got := buildSummarizerSystem("自定义{max_chars}", "旧", "", "", "", 5000)
	if got != "自定义5000" {
		t.Errorf("override = %q, want 自定义5000", got)
	}
	if strings.Contains(got, "<previous-summary>") || strings.Contains(got, "## 目标") {
		t.Errorf("override 不应含模板/previous-summary：%s", got)
	}
}

// §P0-A：stripSummaryPrefix 表驱动（前缀仅展示层，识别必须用 Kind==miniagent.KindSummary）。
func TestStripSummaryPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{summaryPrefix + "x", "x"},
		{"x", "x"},
		{"", ""},
		{summaryPrefix, ""},
	}
	for i, c := range cases {
		if got := stripSummaryPrefix(c.in); got != c.want {
			t.Errorf("case %d: stripSummaryPrefix(%q) = %q, want %q", i, c.in, got, c.want)
		}
	}
}

// §P0-A：默认路径（SummarizerPrompt=""）检测到 head 是旧 miniagent.KindSummary 时，抽 prevSummary、
// 不并入 middle、head 置空。旧摘要文本经 previousSummary 下传（UPDATE 模式）。
func TestCompactWithSummary_UpdateModeExtractsPrevSummary(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model: "m",
		Summarize: func(_ context.Context, _, _ string, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotPrev = prevSummary
			gotMiddle = middle
			return "新摘要", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + "旧摘"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "本轮"},
	}
	out, summary, _, err := compactWithSummary(context.Background(), budget, msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	if gotPrev != "旧摘" {
		t.Errorf("previousSummary = %q, want 旧摘", gotPrev)
	}
	for _, m := range gotMiddle {
		if m.Kind == miniagent.KindSummary {
			t.Errorf("默认路径 middle 不应含 miniagent.KindSummary（旧摘要应作 prevSummary 下传）：%+v", gotMiddle)
		}
	}
	// head 置空：out = summaryMsg + tail（3），首条为新 summary。
	if len(out) != 1+3 || out[0].Kind != miniagent.KindSummary {
		t.Errorf("out 应为 summary+tail（head 已置空）：%+v", out)
	}
}

// §P0-A：override 路径（SummarizerPrompt!=""）维持旧行为——旧 miniagent.KindSummary 并入 middle，
// previousSummary 传空。
func TestCompactWithSummary_OverrideMergesPrevSummaryIntoMiddle(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model:            "m",
		SummarizerPrompt: "自定义{max_chars}",
		Summarize: func(_ context.Context, _, _ string, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotPrev = prevSummary
			gotMiddle = middle
			return "新摘要", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + "旧摘"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "本轮"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 3); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotPrev != "" {
		t.Errorf("override 路径 previousSummary 应为空，got %q", gotPrev)
	}
	if len(gotMiddle) == 0 || gotMiddle[0].Kind != miniagent.KindSummary || !strings.Contains(gotMiddle[0].Content, "旧摘") {
		t.Errorf("override 路径 middle 首条应为旧 miniagent.KindSummary（旧行为）：%+v", gotMiddle)
	}
}

// §P0-A：summarizeMiddle UPDATE 模式把 previous-summary 写进请求 system。
func TestSummarizeMiddle_UpdateModeRequest(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("更新摘要")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "旧锚点", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// lastBody 是 JSON-marshaled 请求体，< > 被转义成 < >；断言用未转义的标签名 + 旧锚点文本。
	if !strings.Contains(tr.lastBody, "previous-summary") || !strings.Contains(tr.lastBody, "旧锚点") {
		t.Errorf("UPDATE 模式请求应含 previous-summary 块 + 旧锚点：%s", tr.lastBody)
	}
}
