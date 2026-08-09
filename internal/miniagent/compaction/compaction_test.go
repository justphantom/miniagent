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

// testBudget builds ContextBudget from llm: Summarize callback calls summarizeMiddle (maxChars=built-in upper bound).
// Model/CompactionModel/System/Tools left zero (these tests don't care about token estimation window, call compactWithSummary directly).
func testBudget(llm *openai.ChatClient) ContextBudget {
	return ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, "", "", "", summaryMaxChars, 0, middle)
		},
	}
}

// jointTailBudget: CW<=0 falls back to preserveRecentTokens(=0); otherwise min(CW×4/5 − reqOverhead − headAdj − summaryEstimate, userCap).
// headAdj is 0 on default path since old summary doesn't enter out. reqOverhead=EstimateTokens(nil,"",nil)=SystemOverhead=400;
// head="q" is non-summary → estimateRoundTokens=4; summaryEstimate=summaryMaxChars/2+Envelope=2504.
func TestJointTailBudget(t *testing.T) {
	head := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}                                           // first round non-summary
	headSum := []miniagent.Message{{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "old summary"}} // UPDATE path head
	mk := func(cw int) ContextBudget { return ContextBudget{ContextWindow: cw, System: "", Tools: nil} }
	cases := []struct {
		name string
		bud  ContextBudget
		head []miniagent.Message
		want int
	}{
		{"CW<=0 no window fall back to 0", mk(0), head, 0},                    // preserveRecentTokens(CW<=0)=0
		{"large CW 128k takes userCap 8000", mk(128000), head, 8000},          // avail 99492 > cap 8000
		{"large CW 128k UPDATE head no deduction", mk(128000), headSum, 8000}, // headAdj=0 but cap dominates
		{"medium CW 5120 deduct head", mk(5120), head, 1188},                  // 4096-400-4-2504=1188 < cap 2000
		{"medium CW 5120 UPDATE no head deduction", mk(5120), headSum, 1192},  // 4096-400-0-2504=1192
		{"small CW 2048 avail<=0 zeroed", mk(2048), head, 0},                  // 1638-400-4-2504<0 → 0
	}
	for _, c := range cases {
		if got := jointTailBudget(c.bud, c.head); got != c.want {
			t.Errorf("%s: jointTailBudget=%d, want %d", c.name, got, c.want)
		}
	}
}

// FitHistory joint budget (§B): CW=5120 + summaryMaxChars=5000 medium window, current (independent tail budget)
// head+summary+tail exceeds window, still exceeds after trim → terminate error; joint budget lets tail yield → out est< CW×4/5, err==nil.
// 20 rounds × 600 Chinese chars (≈6480 token > gate 4096 triggers summary), fake summary callback returns full 5000 chars.
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
	out, _, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("CW=5120 joint budget should not terminate, err=%v (out=%d msgs, summarized=%v)", err, len(out), summarized)
	}
	if !summarized {
		t.Fatal("expected summary compaction to trigger")
	}
}

// FitHistory compaction: the post-compaction tail applies the steady-state P1 reasoning strip — only the most-recent
// keepReasoning (=1) assistant keeps its reasoning (R2/thinking2); earlier tail reasoning (R1) is cleared. committed=true.
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
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R1", Reasoning: "thinking1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q1"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "R2", Reasoning: "thinking2"})
	msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q2"})
	out, _, _, committed, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	reasoningCnt := 0
	keptReasoning := ""
	for _, m := range out {
		if m.Role == miniagent.RoleAssistant && m.Reasoning != "" {
			reasoningCnt++
			keptReasoning = m.Reasoning
		}
	}
	// Post-compaction-tail strip: the tail applies the same steady-state P1 reasoning strip, keeping only the most-recent
	// keepReasoning (=1) assistant's reasoning and clearing earlier tail reasoning (matches next-turn P1).
	if reasoningCnt != 1 {
		t.Errorf("compaction tail should keep exactly 1 (most-recent) reasoning after the post-compaction strip, actual %d", reasoningCnt)
	}
	if keptReasoning != "thinking2" {
		t.Errorf("most-recent reasoning (R2/thinking2) should survive the strip, kept %q", keptReasoning)
	}
	if !committed {
		t.Error("compaction should set committed=true")
	}
}

// FitHistory non-compaction: committed=false (strip is per-round View only, transcript retains original and is not replaced).
func TestFitHistory_NonCompactNotCommitted(t *testing.T) {
	budget := ContextBudget{
		ContextWindow: 128000,
		Model:         "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return "s", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q1"}, {Role: miniagent.RoleUser, Content: "q2"}}
	_, _, _, committed, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if committed {
		t.Error("non-compaction should be committed=false (strip is per-round View only, does not replace transcript)")
	}
}

// deriveSummaryMaxChars (direction A): configured>0 overrides; cw<=0 falls back to 5000; otherwise min(5000, cw/5).
func TestDeriveSummaryMaxChars(t *testing.T) {
	cases := []struct {
		cw, configured, want int
	}{
		{4096, 0, 819},     // small window scales
		{2048, 0, 409},     // even smaller window
		{24999, 0, 4999},   // just below built-in upper bound
		{25000, 0, 5000},   // boundary cw/5=5000 not <5000 -> built-in
		{40000, 0, 5000},   // large window clamps to built-in
		{0, 0, 5000},       // no window falls back to built-in
		{4096, 3000, 3000}, // user explicit override
	}
	for _, c := range cases {
		if got := deriveSummaryMaxChars(c.cw, c.configured); got != c.want {
			t.Errorf("deriveSummaryMaxChars(%d, %d) = %d, want %d", c.cw, c.configured, got, c.want)
		}
	}
}

// NewCompaction: when SummaryMaxChars is unset, maxChars scales with ContextWindow (direction A), maxSummaryTokens auto-follows.
// CW=4096 -> maxChars=819 -> maxSummaryTokens=819/2=409. 20 rounds x 600 CJK chars triggers summary; ignore before's possible terminate error,
// only verify the summary request carries the derived max_tokens (under A+B CW=4096 does not terminate in practice, but assertion does not depend on this).
func TestNewCompaction_ScalesSummaryMaxCharsByWindow(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	before, _ := NewCompaction(CompactionOptions{
		Chat:          llm,
		ContextWindow: 4096, // SummaryMaxChars unset -> derives 819 -> maxSummaryTokens 409
	})
	var msgs []miniagent.Message
	for range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: strings.Repeat("历", 600)})
	}
	_, _ = before(context.Background(), miniagent.StepInput{Step: 1, Msgs: msgs})
	if tr.calls == 0 {
		t.Fatal("expected summary LLM call to be triggered")
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":409`) {
		t.Errorf("expected max_tokens=819/2=409 (summaryMaxChars scales with CW), actual: %s", tr.lastBody)
	}
}

// compactWithSummary strips middle fully before summarizing: captures the middle received by Summarize, asserts reasoning cleared + read deduped + pairing intact.
func TestCompactWithSummary_StripsMiddleBeforeSummarize(t *testing.T) {
	var captured []miniagent.Message
	budget := ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			captured = middle
			return "summary", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "head first round"}}
	for i := range 6 {
		id := "c" + strconv.Itoa(i)
		msgs = append(msgs, miniagent.Message{
			Role:      miniagent.RoleAssistant,
			Content:   "read file",
			Reasoning: strings.Repeat("z", 800),
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
			t.Errorf("middle reasoning should be fully cleared before summarizing, still has %d runes", len([]rune(m.Reasoning)))
		}
	}
	// dedup (P6) now takes effect (windowStartOf(0)=len, fully outside window): of 6 reads with same path/offset, keep the last in time order, earlier ones become placeholders.
	deduped := 0
	for _, m := range captured {
		if m.Role == miniagent.RoleTool && strings.Contains(m.Content, "superseded by a more recent read") {
			deduped++
		}
	}
	if deduped == 0 {
		t.Errorf("expected same path/offset reads to be deduped to placeholders, but captured tool has none")
	}
	// After reasoning cleared (~2400 tokens) + read dedup (6->1), size should be < 1500.
	capturedTokens := policy.EstimateTokens(captured, "", nil)
	if capturedTokens > 1500 {
		t.Errorf("middle size after strip should be < 1500 (reasoning cleared + read dedup), actual %d", capturedTokens)
	}
	if err := session.ValidateToolPairing(out); err != nil {
		t.Errorf("pairing broken after strip: %v", err)
	}
}

// windowStartOf: keepN<=0 -> len(msgs) (fully outside window = all stripped); keepN assistant entries exist -> index of the keepN-th (from end); fewer -> 0.
func TestWindowStartOf(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser},
		{Role: miniagent.RoleAssistant},
		{Role: miniagent.RoleUser},
		{Role: miniagent.RoleAssistant},
		{Role: miniagent.RoleUser},
	} // assistant at 1,3; len=5
	cases := []struct{ keepN, want int }{
		{0, 5},  // fully outside window (all stripped)
		{-1, 5}, // same
		{1, 3},  // most recent 1 assistant=index3
		{2, 1},  // most recent 2=index1
		{3, 0},  // fewer than 3 assistant (only 2) -> 0 (fully inside window = all kept)
	}
	for _, c := range cases {
		if got := windowStartOf(msgs, c.keepN); got != c.want {
			t.Errorf("windowStartOf(keepN=%d)=%d, want %d", c.keepN, got, c.want)
		}
	}
}

// applyCompactionBarrier: has summary -> return latest summary and after; none -> as-is.
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

// compactWithSummary: middle summarized into miniagent.KindSummary, structure (earliest 1 round + summary + most-recent N rounds) correct,
// and summary can be merged into newMsgs (persistence semantics; production path is loop.go mergePersisted).
func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("compacted summary")}}
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
	// Earliest 1 round + summary + most-recent 3 rounds
	if len(out) != 1+1+3 {
		t.Errorf("out len = %d, want 5", len(out))
	}
	if out[1].Kind != miniagent.KindSummary || !strings.Contains(out[1].Content, "compacted summary") {
		t.Errorf("summary slot wrong: %+v", out[1])
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	if len(newMsgs) != 1 || newMsgs[0].Kind != miniagent.KindSummary {
		t.Errorf("summary not persisted to newMsgs: %+v", newMsgs)
	}
}

// Middle pairing break (orphan tool message) -> not summarized, returns error (caller falls back to lossy).
func TestCompactWithSummary_PairingBreakErrors(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleTool, ToolCallID: "orphan", Content: "x"}, // broken pairing
		{Role: miniagent.RoleUser, Content: "u2"},
		{Role: miniagent.RoleUser, Content: "u3"},
		{Role: miniagent.RoleUser, Content: "u4"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 1); err == nil {
		t.Fatal("expected pairing-break error")
	}
}

// No middle to summarize (rounds <= 1+keepRecent) -> summary.Kind=="", no LLM call, msgs unchanged.
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

// summarizeMiddle LLM error propagates (not swallowed).
func TestSummarizeMiddle_LLMError(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}

// P2 summary tokens enter budget: summarizeMiddle returns LLM usage for upstream accumulation into MaxTotalTokens budget.
func TestSummarizeMiddle_ReturnsUsage(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"## Goal: usage probe\n\n## Progress: usage probe"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":30}}`
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

// P3-1: summary request sets MaxTokens=summaryMaxTokens (derived by default from summaryMaxChars = summaryMaxChars/2).
func TestSummarizeMiddle_SetsMaxTokens(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// Reference constant rather than magic number: summaryMaxTokens now derived from summaryMaxChars/2, auto-follows future chars changes.
	if !strings.Contains(tr.lastBody, `"max_tokens":`+strconv.Itoa(summaryMaxTokens)) {
		t.Errorf("summary request did not set max_tokens=%d: %s", summaryMaxTokens, tr.lastBody)
	}
}

// deriveSummaryMaxTokens: configured>0 overrides; otherwise derived from maxChars (chars/2, densest CJK calibration); maxChars<2 falls back to fallback constant.
func TestDeriveSummaryMaxTokens(t *testing.T) {
	cases := []struct {
		maxChars, configured, want int
	}{
		{5000, 0, 2500},          // default derived (summaryMaxChars/2)
		{5000, 512, 512},         // explicit user override
		{8000, 0, 4000},          // only chars configured -> token follows
		{0, 0, summaryMaxTokens}, // maxChars<=0 defensive fallback
		{1, 0, summaryMaxTokens}, // maxChars<2 fallback
	}
	for _, c := range cases {
		if got := deriveSummaryMaxTokens(c.maxChars, c.configured); got != c.want {
			t.Errorf("deriveSummaryMaxTokens(%d, %d) = %d, want %d", c.maxChars, c.configured, got, c.want)
		}
	}
}

// NewCompaction: only SummaryMaxChars configured (no token) -> summary request max_tokens derived from chars (chars/2).
// Verifies the "configure chars -> token auto-follows" end-to-end contract. CW=1 forces history to exceed 4/5 gate triggering summary; >0 tokens post-compaction
// will hit FitHistory termination guard (returns error) — but the summary LLM call already happened before termination, tr.lastBody is already recorded, so the error is ignored.
func TestNewCompaction_DerivesMaxTokensFromChars(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	before, after := NewCompaction(CompactionOptions{
		Chat:            llm,
		ContextWindow:   1,
		SummaryMaxChars: 8000, // SummaryMaxTokens unset -> derived 8000/2=4000
	})
	if after != nil {
		t.Fatalf("after should be nil (overflow detection merged into before), got non-nil")
	}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	// CW=1 triggers summary then hits termination guard: ignore error, verify derivation via tr.calls/lastBody.
	_, _ = before(context.Background(), miniagent.StepInput{Step: 1, Msgs: msgs})
	if tr.calls == 0 {
		t.Fatal("expected summary LLM call to be triggered")
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":4000`) {
		t.Errorf("expected max_tokens=8000/2=4000 (chars-derived), got: %s", tr.lastBody)
	}
}

// compactWithSummary should propagate budget.CompactionModel to the Summarize callback.
func TestCompactWithSummary_CompactionModelOverride(t *testing.T) {
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{summaryResponse("x")}}}}
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

// §P0-A: buildSummarizerSystem three modes. CREATE: contains template 6 sections, no <previous-summary>.
func TestBuildSummarizerSystem_CreateMode(t *testing.T) {
	got := buildSummarizerSystem("", "", "", "", "", 5000)
	for _, want := range []string{"## Goal", "## Key Details", "## Progress", "## Next Step", "## Relevant Files"} {
		if !strings.Contains(got, want) {
			t.Errorf("CREATE mode should contain template section %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<previous-summary>") {
		t.Errorf("CREATE mode should not contain <previous-summary>: %s", got)
	}
}

// §P0-A: UPDATE: contains <previous-summary> block wrapping old summary + update instruction + template 6 sections.
func TestBuildSummarizerSystem_UpdateMode(t *testing.T) {
	got := buildSummarizerSystem("", "old-anchor", "", "", "", 5000)
	for _, want := range []string{"<previous-summary>\nold-anchor\n</previous-summary>", "update the existing anchored summary", "## Goal", "## Relevant Files"} {
		if !strings.Contains(got, want) {
			t.Errorf("UPDATE mode should contain %q:\n%s", want, got)
		}
	}
}

// §P0-A: override: summarizerPrompt non-empty -> renders {max_chars}; with a non-empty previousSummary and no {previous_summary}
// placeholder, the <previous-summary> block is appended (override is a superset of default, never loses the old summary).
// The summaryTemplate is still NOT appended in override.
func TestBuildSummarizerSystem_Override(t *testing.T) {
	got := buildSummarizerSystem("custom{max_chars}", "old", "", "", "", 5000)
	if !strings.HasPrefix(got, "custom5000") {
		t.Errorf("override should render {max_chars}: %q", got)
	}
	if !strings.Contains(got, "<previous-summary>") || !strings.Contains(got, "</previous-summary>") || !strings.Contains(got, "old") {
		t.Errorf("override with non-empty previousSummary should append the <previous-summary> block: %q", got)
	}
	if strings.Contains(got, "## Goal") {
		t.Errorf("override should not contain the summaryTemplate: %q", got)
	}
}

// §P0-A: stripSummaryPrefix table-driven (prefix is presentation-only, identification must use Kind==miniagent.KindSummary).
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

// §P0-A: default path (SummarizerPrompt="") detects head is an old miniagent.KindSummary, extracts prevSummary,
// does not merge into middle, sets head to empty. Old summary text passed down via previousSummary (UPDATE mode).
func TestCompactWithSummary_UpdateModeExtractsPrevSummary(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model: "m",
		Summarize: func(_ context.Context, _, _ string, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotPrev = prevSummary
			gotMiddle = middle
			return "new-summary", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + "old-sum"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "this-turn"},
	}
	out, summary, _, err := compactWithSummary(context.Background(), budget, msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	if gotPrev != "old-sum" {
		t.Errorf("previousSummary = %q, want old-sum", gotPrev)
	}
	for _, m := range gotMiddle {
		if m.Kind == miniagent.KindSummary {
			t.Errorf("default path middle should not contain miniagent.KindSummary (old summary should be passed down as prevSummary): %+v", gotMiddle)
		}
	}
	// head set to empty: out = summaryMsg + tail (3), first is new summary.
	if len(out) != 1+3 || out[0].Kind != miniagent.KindSummary {
		t.Errorf("out should be summary+tail (head set to empty): %+v", out)
	}
}

// §P0-A: summarizeMiddle UPDATE mode writes previous-summary into request system.
func TestSummarizeMiddle_UpdateModeRequest(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("updated-summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "old-anchor", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// lastBody is JSON-marshaled request body, < > are escaped to &lt; &gt;; assert with unescaped tag name + old-anchor text.
	if !strings.Contains(tr.lastBody, "previous-summary") || !strings.Contains(tr.lastBody, "old-anchor") {
		t.Errorf("UPDATE mode request should contain previous-summary block + old-anchor: %s", tr.lastBody)
	}
}
