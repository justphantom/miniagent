package compaction

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/openai"
)

func testBudget(llm *openai.ChatClient) ContextBudget {
	return ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, "", "", "", summaryMaxChars, 0, middle)
		},
	}
}

func TestJointTailBudget(t *testing.T) {
	head := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}                                           // first round non-summary
	headSum := []miniagent.Message{{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "old summary"}} // UPDATE path head
	mk := func(cw int) ContextBudget { return ContextBudget{ContextWindow: cw, System: "", Tools: nil} }
	mkOverride := func(cw int) ContextBudget { b := mk(cw); b.SummarizerPrompt = "custom"; return b } // override path (custom summarizer_prompt)
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
		// override path (SummarizerPrompt!=""): a single KindSummary head is extracted in BOTH default and override paths,
		// so headAdj=0 here too. Regression guard: a stale `SummarizerPrompt != ""` clause once wrongly deducted the old
		// summary here, driving tail budget to 0 and failing small-window compaction under a custom summarizer_prompt.
		{"medium CW 5120 override + summary head no deduction", mkOverride(5120), headSum, 1192}, // same as default UPDATE head
		{"medium CW 5120 override + non-summary head deducts", mkOverride(5120), head, 1188},     // non-summary head still enters out → deduct
	}
	for _, c := range cases {
		if got := jointTailBudget(c.bud, c.head); got != c.want {
			t.Errorf("%s: jointTailBudget=%d, want %d", c.name, got, c.want)
		}
	}
}

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
