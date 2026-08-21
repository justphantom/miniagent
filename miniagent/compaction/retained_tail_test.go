package compaction

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/openai"
)

// msgsContainContent reports whether any Content in msgs contains sub.
func msgsContainContent(msgs []miniagent.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

// §P1-E preserveRecentTokens: explicit >0 / window<=0 disabled / floor(window/4) clamp [2000,8000].
func TestPreserveRecentTokens(t *testing.T) {
	cases := []struct{ preserve, window, want int }{
		{5000, 0, 5000},   // explicit >0 wins
		{0, 0, 0},         // window<=0 -> disabled
		{0, 8000, 2000},   // 8000/4
		{0, 20000, 5000},  // 20000/4
		{0, 100000, 8000}, // 25000 clamp upper limit
		{0, 4000, 2000},   // 1000 clamp lower limit
	}
	for i, c := range cases {
		b := ContextBudget{PreserveRecentTokens: c.preserve, ContextWindow: c.window}
		if got := preserveRecentTokens(b); got != c.want {
			t.Errorf("case %d: preserveRecentTokens = %d, want %d", i, got, c.want)
		}
	}
}

// §P1-E selectTailByTokens token budget: a large round that doesn't fit goes into middle (when boundary shrink fails); the recent small round is kept in tail.
func TestSelectTailByTokens_TokenBudget(t *testing.T) {
	bigTool := strings.Repeat("x", 20000) // ~5000 tokens
	rounds := [][]miniagent.Message{
		{{Role: miniagent.RoleUser, Content: "a"}},
		{{Role: miniagent.RoleUser, Content: "b"}},
		{{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "t", Args: "{}"}}, Content: ""},
			{Role: miniagent.RoleTool, ToolCallID: "c1", Content: bigTool}},
		{{Role: miniagent.RoleUser, Content: "d"}}, // recent
	}
	tail, middle := selectTailByTokens(rounds, 4, 50)
	if !msgsContainContent(tail, "d") {
		t.Errorf("tail should contain the recent small round d: %+v", tail)
	}
	if msgsContainContent(tail, "a") {
		t.Errorf("the ancient small round a should not be in tail (should go into middle): %+v", tail)
	}
	if !msgsContainContent(middle, "a") {
		t.Errorf("middle should contain the ancient small round a: %+v", middle)
	}
	if !msgsContainContent(middle, bigTool[:50]) {
		t.Errorf("middle should contain the large round tool content (doesn't fit): tail=%d middle=%d", len(tail), len(middle))
	}
}

// §P1-E selectTailByTokens pure turn-count fallback (tokenBudget<=0): tail=the most recent maxTurns rounds, middle=the rest.
func TestSelectTailByTokens_LegacyFallback(t *testing.T) {
	rounds := [][]miniagent.Message{
		{{Role: miniagent.RoleUser, Content: "a"}},
		{{Role: miniagent.RoleUser, Content: "b"}},
		{{Role: miniagent.RoleUser, Content: "c"}},
		{{Role: miniagent.RoleUser, Content: "d"}},
		{{Role: miniagent.RoleUser, Content: "e"}},
	}
	tail, middle := selectTailByTokens(rounds, 2, 0)
	if len(tail) != 2 || !msgsContainContent(tail, "d") || !msgsContainContent(tail, "e") {
		t.Errorf("tail should be the most recent 2 rounds [d,e]: %+v", tail)
	}
	if len(middle) != 3 || !msgsContainContent(middle, "a") || !msgsContainContent(middle, "c") {
		t.Errorf("middle should be [a,b,c]: %+v", middle)
	}
}

// §P1-E selectTailByTokens all-fit: all rounds fit (n<=maxTurns and the token upper bound not hit) -> tail=all, middle=empty.
func TestSelectTailByTokens_AllFit(t *testing.T) {
	rounds := [][]miniagent.Message{
		{{Role: miniagent.RoleUser, Content: "a"}},
		{{Role: miniagent.RoleUser, Content: "b"}},
	}
	tail, middle := selectTailByTokens(rounds, 5, 1000)
	if len(tail) != 2 || !msgsContainContent(tail, "a") || !msgsContainContent(tail, "b") {
		t.Errorf("all-fit: tail should be all 2 rounds: %+v", tail)
	}
	if len(middle) != 0 {
		t.Errorf("all-fit: middle should be empty: %+v", middle)
	}
}

// selectTailByTokens invariant: when the most recent round alone exceeds tokenBudget, it is still force-merged
// into tail and not squeezed into middle to be summarized.
// Regression: previously, if the most recent round's bulk was in assistant.tool_call.Args (e.g. write a large file,
// where shrinkRoundToolContents cannot compress it), boundary split/shrink would both fail -> tail empty ->
// compactWithSummary would merge the most recent round into middle and summarize it, losing precise recent context.
func TestSelectTailByTokens_RecentRoundExceedsBudget(t *testing.T) {
	bigArgs := strings.Repeat("x", 20000) // in tool_call.Args; shrinkRoundToolContents does not touch it -> cannot fit the budget
	rounds := [][]miniagent.Message{
		{{Role: miniagent.RoleUser, Content: "old"}},
		{ // recent round: bulk is in assistant.tool_call.Args, shrink is ineffective
			{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "write", Args: bigArgs}}},
			{Role: miniagent.RoleTool, ToolCallID: "c1", Content: "ok"},
		},
	}
	tail, middle := selectTailByTokens(rounds, 4, 50)
	if len(tail) == 0 {
		t.Fatalf("tail must not be empty: even if the most recent round alone exceeds the budget it must be merged into tail")
	}
	// The recent round (containing c1) should be in tail, not in middle.
	hasC1 := false
	for _, m := range tail {
		if m.ToolCallID == "c1" {
			hasC1 = true
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "c1" {
				hasC1 = true
			}
		}
	}
	if !hasC1 {
		t.Errorf("the recent round (c1) should stay in tail and not be summarized: tail=%+v", tail)
	}
	if !msgsContainContent(middle, "old") {
		t.Errorf("the older round should go into middle: %+v", middle)
	}
	if err := miniagent.ValidateToolPairing(tail); err != nil {
		t.Errorf("tail tool-call pairing should be self-consistent: %v", err)
	}
}

// §P1-E shrinkRoundToolContents: keeps assistant.tool_calls and tool result pairing unchanged; only tool content is truncated.
func TestShrinkRoundToolContents_PairingPreserved(t *testing.T) {
	round := []miniagent.Message{
		{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "t", Args: "{}"}}, Content: ""},
		{Role: miniagent.RoleTool, ToolCallID: "c1", Content: strings.Repeat("x", 8000)},
	}
	shrunk := shrinkRoundToolContents(round, 200)
	if len(shrunk) != 2 {
		t.Fatalf("the round length after shrink should be unchanged (2 items): %d", len(shrunk))
	}
	if len(shrunk[0].ToolCalls) != 1 || shrunk[0].ToolCalls[0].ID != "c1" {
		t.Errorf("assistant.tool_calls should be kept as-is: %+v", shrunk[0].ToolCalls)
	}
	if shrunk[1].Role != miniagent.RoleTool || shrunk[1].ToolCallID != "c1" {
		t.Errorf("the tool result should keep its id pairing: %+v", shrunk[1])
	}
	if len(shrunk[1].Content) >= 8000 {
		t.Errorf("tool content should be compressed: len=%d", len(shrunk[1].Content))
	}
	if err := miniagent.ValidateToolPairing(shrunk); err != nil {
		t.Errorf("pairing should be self-consistent after shrink: %v", err)
	}
}

// §P1-E compactWithSummary end-to-end: the token budget becomes a binding constraint -- when the most recent
// round contains a huge tool result (> the budget), that round does not go into tail (it goes into middle and is
// summarized). Guards the preserveRecentTokens(budget) wiring (review Finding 2): if tokenBudget is wrongly changed
// to 0 (pure turn-count fallback), tail=the most recent keepRecent rounds would contain bigTool, failing this assertion.
func TestCompactWithSummary_TokenBudgetTailE2E(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("sum")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	bigTool := strings.Repeat("x", 20000) // ~5000 tokens > preserveRecentTokens=2000
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "h0"},
		{Role: miniagent.RoleUser, Content: "h1"},
		{Role: miniagent.RoleUser, Content: "h2"},
		{Role: miniagent.RoleUser, Content: "h3"},
		{Role: miniagent.RoleUser, Content: "h4"},
		{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "t", Args: "{}"}}, Content: ""},
		{Role: miniagent.RoleTool, ToolCallID: "c1", Content: bigTool},
		{Role: miniagent.RoleUser, Content: "cur"},
	}
	budget := ContextBudget{
		Model:         "m",
		ContextWindow: 8000, // preserveRecentTokens = floor(8000/4)=2000 -> clamp[2000,8000]=2000
		Summarize:     testBudget(llm).Summarize,
	}
	out, summary, _, err := compactWithSummary(context.Background(), budget, msgs, 4)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("should generate miniagent.KindSummary: kind=%v err=%v", summary.Kind, err)
	}
	if len(out) == 0 || out[0].Content != "h0" {
		t.Errorf("out[0] should be the head h0: %+v", out)
	}
	// Token budget binding: a huge tool result (5000 tokens > budget 2000) should not go into tail (it goes into middle and is summarized).
	// When tokenBudget=0 falls back, tail=the most recent 4 rounds would contain bigTool, failing this assertion.
	for _, m := range out {
		if strings.Contains(m.Content, bigTool[:50]) {
			t.Fatalf("the token budget should prevent a huge tool result from entering tail (should go into middle): out contains bigTool")
		}
	}
}
