package miniagent

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// msgsContainContent 报告 msgs 中是否有任一 Content 含 sub。
func msgsContainContent(msgs []Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

// §P1-E preserveRecentTokens：显式 >0 / window<=0 关闭 / floor(window/4) clamp [2000,8000]。
func TestPreserveRecentTokens(t *testing.T) {
	cases := []struct{ preserve, window, want int }{
		{5000, 0, 5000},   // 显式 >0 wins
		{0, 0, 0},         // window<=0 → 关闭
		{0, 8000, 2000},   // 8000/4
		{0, 20000, 5000},  // 20000/4
		{0, 100000, 8000}, // 25000 clamp 上限
		{0, 4000, 2000},   // 1000 clamp 下限
	}
	for i, c := range cases {
		b := ContextBudget{PreserveRecentTokens: c.preserve, ContextWindow: c.window}
		if got := preserveRecentTokens(b); got != c.want {
			t.Errorf("case %d: preserveRecentTokens = %d, want %d", i, got, c.want)
		}
	}
}

// §P1-E selectTailByTokens token 预算：大轮装不下进 middle（boundary shrink 失败时），最近小轮留 tail。
func TestSelectTailByTokens_TokenBudget(t *testing.T) {
	bigTool := strings.Repeat("x", 20000) // ~5000 tokens
	rounds := [][]Message{
		{{Role: roleUser, Content: "a"}},
		{{Role: roleUser, Content: "b"}},
		{{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Args: "{}"}}, Content: ""},
			{Role: roleTool, ToolCallID: "c1", Content: bigTool}},
		{{Role: roleUser, Content: "d"}}, // 最近
	}
	tail, middle := selectTailByTokens(rounds, 4, 50)
	if !msgsContainContent(tail, "d") {
		t.Errorf("tail 应含最近小轮 d: %+v", tail)
	}
	if msgsContainContent(tail, "a") {
		t.Errorf("远古小轮 a 不应在 tail（应进 middle）: %+v", tail)
	}
	if !msgsContainContent(middle, "a") {
		t.Errorf("middle 应含远古小轮 a: %+v", middle)
	}
	if !msgsContainContent(middle, bigTool[:50]) {
		t.Errorf("middle 应含大轮 tool content（装不下）: tail=%d middle=%d", len(tail), len(middle))
	}
}

// §P1-E selectTailByTokens 纯轮数回退（tokenBudget<=0）：tail=最近 maxTurns 轮，middle=其余。
func TestSelectTailByTokens_LegacyFallback(t *testing.T) {
	rounds := [][]Message{
		{{Role: roleUser, Content: "a"}},
		{{Role: roleUser, Content: "b"}},
		{{Role: roleUser, Content: "c"}},
		{{Role: roleUser, Content: "d"}},
		{{Role: roleUser, Content: "e"}},
	}
	tail, middle := selectTailByTokens(rounds, 2, 0)
	if len(tail) != 2 || !msgsContainContent(tail, "d") || !msgsContainContent(tail, "e") {
		t.Errorf("tail 应为最近 2 轮 [d,e]: %+v", tail)
	}
	if len(middle) != 3 || !msgsContainContent(middle, "a") || !msgsContainContent(middle, "c") {
		t.Errorf("middle 应为 [a,b,c]: %+v", middle)
	}
}

// §P1-E selectTailByTokens all-fit：全部轮装下（n<=maxTurns 且未触 token 上界）→ tail=全部、middle=空。
func TestSelectTailByTokens_AllFit(t *testing.T) {
	rounds := [][]Message{
		{{Role: roleUser, Content: "a"}},
		{{Role: roleUser, Content: "b"}},
	}
	tail, middle := selectTailByTokens(rounds, 5, 1000)
	if len(tail) != 2 || !msgsContainContent(tail, "a") || !msgsContainContent(tail, "b") {
		t.Errorf("all-fit: tail 应为全部 2 轮: %+v", tail)
	}
	if len(middle) != 0 {
		t.Errorf("all-fit: middle 应为空: %+v", middle)
	}
}

// §P1-E splitRoundByTokens 配对安全：tool-call 轮返回 nil（不可安全切）；多消息文本轮切在消息边界。
func TestSplitRoundByTokens_PairingSafe(t *testing.T) {
	// tool-call 轮：[A(tc=[c1,c2]), T(c1), T(c2)] → 不可切（切点后缀不能以 tool 开头）。
	tcRound := []Message{
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Args: "{}"}, {ID: "c2", Name: "t", Args: "{}"}}},
		{Role: roleTool, ToolCallID: "c1", Content: "r1"},
		{Role: roleTool, ToolCallID: "c2", Content: "r2"},
	}
	if got := splitRoundByTokens(tcRound, 100); got != nil {
		t.Errorf("tool-call 轮应返回 nil（不可安全切）: %+v", got)
	}
	// 多消息文本轮（手动构造）：切点落在使后缀 fit 的最早消息边界。
	textRound := []Message{
		{Role: roleUser, Content: "x"},
		{Role: roleUser, Content: "y"},
		{Role: roleUser, Content: "z"},
	}
	suffix := splitRoundByTokens(textRound, estimateRoundTokens([]Message{{Role: roleUser, Content: "z"}}))
	if len(suffix) != 1 || suffix[0].Content != "z" {
		t.Errorf("文本轮应切出后缀 [z]: %+v", suffix)
	}
}

// §P1-E shrinkRoundToolContents：保 assistant.tool_calls 与 tool 结果配对不变，仅 tool content 被截。
func TestShrinkRoundToolContents_PairingPreserved(t *testing.T) {
	round := []Message{
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Args: "{}"}}, Content: ""},
		{Role: roleTool, ToolCallID: "c1", Content: strings.Repeat("x", 8000)},
	}
	shrunk := shrinkRoundToolContents(round, 200)
	if len(shrunk) != 2 {
		t.Fatalf("shrink 后轮长度应不变（2 条）: %d", len(shrunk))
	}
	if len(shrunk[0].ToolCalls) != 1 || shrunk[0].ToolCalls[0].ID != "c1" {
		t.Errorf("assistant.tool_calls 应原样保留: %+v", shrunk[0].ToolCalls)
	}
	if shrunk[1].Role != roleTool || shrunk[1].ToolCallID != "c1" {
		t.Errorf("tool 结果应保留 id 配对: %+v", shrunk[1])
	}
	if len(shrunk[1].Content) >= 8000 {
		t.Errorf("tool content 应被压缩: len=%d", len(shrunk[1].Content))
	}
	if err := validateToolPairing(shrunk); err != nil {
		t.Errorf("shrink 后配对应自洽: %v", err)
	}
}

// §P1-E compactWithSummary 端到端：token 预算成为绑定约束——最近轮含超大 tool 结果（> 预算）时，
// 该轮不进 tail（进 middle 被摘要）。守护 preserveRecentTokens(budget) wiring（review Finding 2）：
// 若 tokenBudget 误改成 0（纯轮数回退），tail=最近 keepRecent 轮会含 bigTool，此断言失败。
func TestCompactWithSummary_TokenBudgetTailE2E(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("sum")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	bigTool := strings.Repeat("x", 20000) // ~5000 tokens > preserveRecentTokens=2000
	msgs := []Message{
		{Role: roleUser, Content: "h0"},
		{Role: roleUser, Content: "h1"},
		{Role: roleUser, Content: "h2"},
		{Role: roleUser, Content: "h3"},
		{Role: roleUser, Content: "h4"},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Args: "{}"}}, Content: ""},
		{Role: roleTool, ToolCallID: "c1", Content: bigTool},
		{Role: roleUser, Content: "cur"},
	}
	budget := ContextBudget{
		Model:         "m",
		ContextWindow: 8000, // preserveRecentTokens = floor(8000/4)=2000 → clamp[2000,8000]=2000
		Summarize:     testBudget(llm).Summarize,
	}
	out, summary, _, err := compactWithSummary(context.Background(), budget, msgs, 4)
	if err != nil || summary.Kind != KindSummary {
		t.Fatalf("应生成 KindSummary: kind=%v err=%v", summary.Kind, err)
	}
	if len(out) == 0 || out[0].Content != "h0" {
		t.Errorf("out[0] 应为 head h0: %+v", out)
	}
	// token 预算绑定：超大 tool 结果（5000 tokens > 预算 2000）不应进 tail（应进 middle 摘要）。
	// tokenBudget=0 回退时 tail=最近 4 轮会含 bigTool，此断言失败。
	for _, m := range out {
		if strings.Contains(m.Content, bigTool[:50]) {
			t.Fatalf("token 预算应阻止超大 tool 结果进 tail（应进 middle）: out 含 bigTool")
		}
	}
}
