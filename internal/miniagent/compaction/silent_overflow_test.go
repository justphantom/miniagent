package compaction

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// §P1-B isUsageOverflow table-driven (B.5 case set). maxTokens no longer participates in the reserve (see
// compaction_split.go compactionBuffer): the detector fires at ~80% of CW, not ~97%.
func TestIsUsageOverflow(t *testing.T) {
	cases := []struct {
		name               string
		u                  miniagent.Usage
		contextWindow, res int
		auto               bool
		want               bool
	}{
		{"auto_off", miniagent.Usage{InputTokens: 99000, OutputTokens: 0}, 100000, 0, false, false},
		{"window_zero", miniagent.Usage{InputTokens: 99000, OutputTokens: 0}, 0, 0, true, false},
		{"under_usable", miniagent.Usage{InputTokens: 79000, OutputTokens: 0}, 100000, 0, true, false},        // usable=80000
		{"equal_usable", miniagent.Usage{InputTokens: 80000, OutputTokens: 0}, 100000, 0, true, true},         // 80000>=80000
		{"over_usable", miniagent.Usage{InputTokens: 81000, OutputTokens: 0}, 100000, 0, true, true},          // 81000>80000
		{"reserved_explicit", miniagent.Usage{InputTokens: 95500, OutputTokens: 0}, 100000, 5000, true, true}, // usable=95000
		{"zero_usage", miniagent.Usage{InputTokens: 0, OutputTokens: 0}, 100000, 0, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUsageOverflow(c.u, c.contextWindow, c.res, c.auto); got != c.want {
				t.Errorf("isUsageOverflow = %v, want %v", got, c.want)
			}
		})
	}
}

// §P1-B compactionReserve two branches (reservedCfg>0 wins; otherwise the flat buffer — the former maxTokens
// branch is gone).
func TestCompactionReserve(t *testing.T) {
	cases := []struct {
		res, want int
	}{
		{500, 500}, // reservedCfg>0 wins
		{0, 20000}, // default → flat buffer
		{30000, 30000},
	}
	for i, c := range cases {
		if got := compactionReserve(c.res); got != c.want {
			t.Errorf("case %d: compactionReserve(%d) = %d, want %d", i, c.res, got, c.want)
		}
	}
}

// §P1-B usableTokens: window<=0→0; large CW subtracts the flat 20000 reserve; small CW clamps reserve to CW/5.
func TestUsableTokens(t *testing.T) {
	cases := []struct {
		contextWindow, res, want int
	}{
		{0, 0, 0},
		{100000, 0, 80000},    // reserve=min(20000,20000)=20000 → 80% (was 95904 when coupled to maxTokens)
		{100000, 5000, 95000}, // reserved explicit: reserve=min(5000,20000)=5000
		{128000, 0, 108000},   // reserve=min(20000,25600)=20000 → 84.4% (big-context model, the bug's main victim)
		{100, 0, 80},          // small CW: CW/5=20 clamp → 100-20=80 (prevents reserve from exceeding half and inverting against the gate)
	}
	for i, c := range cases {
		if got := usableTokens(c.contextWindow, c.res); got != c.want {
			t.Errorf("case %d: usableTokens = %d, want %d", i, got, c.want)
		}
	}
}

// §P1-B usageFootprint = input+output (same metric as contextTokensFromUsage).
func TestUsageFootprint(t *testing.T) {
	if got := usageFootprint(miniagent.Usage{InputTokens: 100, OutputTokens: 50}); got != 150 {
		t.Errorf("usageFootprint = %d, want 150", got)
	}
	if got := usageFootprint(miniagent.Usage{}); got != 0 {
		t.Errorf("usageFootprint zero = %d, want 0", got)
	}
}

// §P1-B When Force=true, even if policy.EstimateTokens is far below the 4/5 threshold, FitHistory still
// goes through compactWithSummary.
func TestFitHistory_ForceCompactsRegardlessOfEstimate(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("forced summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	budget := ContextBudget{
		ContextWindow: 1000000, // 4/5=800000: policy.EstimateTokens(~small) << threshold → normally not compacted
		KeepRecent:    3,
		Force:         true,
		Summarize:     testBudget(llm).Summarize,
	}
	_, summary, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if !summarized || summary.Kind != miniagent.KindSummary {
		t.Errorf("Force=true should trigger compaction (regardless of estimate): summarized=%v kind=%v", summarized, summary.Kind)
	}
}

// §P1-B Integration: the previous step's real usage hits the window → next step forces compaction
// (result.Compacted=true). Requires step1 to return tool_call (so miniagent.Run continues) + huge usage;
// step2 FitHistory(Force) summarizes (consuming one response); step2 final text.
func TestRun_SilentUsageOverflowTriggersCompaction(t *testing.T) {
	tool := miniagent.Tool{Name: "t", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "tr"} }}
	// step1: tool_call + huge prompt_tokens (>= usable=8000: CW=10000 - reserve clamp 2000, bug 6) triggers overflow.
	step1 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8500,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{step1, summaryResponse("compaction-summary"), textResponse("done")}}
	chat, stream := testClients(tr)
	// 6-round history + prompt → step2 compaction has a mid-section to summarize (>1+keepRecent=5).
	history := []miniagent.Message{{Role: miniagent.RoleUser, Content: "h1"}, {Role: miniagent.RoleUser, Content: "h2"}, {Role: miniagent.RoleUser, Content: "h3"}, {Role: miniagent.RoleUser, Content: "h4"}, {Role: miniagent.RoleUser, Content: "h5"}, {Role: miniagent.RoleUser, Content: "h6"}}
	before, after := NewCompaction(CompactionOptions{Chat: chat, ContextWindow: 10000, Auto: true, Model: "m"})
	res, err := miniagent.Run(context.Background(), &openai.Provider{Chat: chat, Stream: stream}, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: history}, "prompt", miniagent.LoopHooks{BeforeLLM: before, AfterLLM: after}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if !res.Compacted {
		t.Error("silent overflow should trigger compaction on the next step (result.Compacted=true)")
	}
	// Don't just check the bool: verify the summary is actually persisted into NewMessages (mergePersisted
	// path), and that multiple compactions leave exactly 1 entry.
	var summaryCount int
	for _, m := range res.NewMessages {
		if m.Kind == miniagent.KindSummary {
			summaryCount++
		}
	}
	if summaryCount != 1 {
		t.Errorf("NewMessages should contain exactly 1 summary (mergePersisted dedup), got %d", summaryCount)
	}
}

// §P1-B Control: CompactionAuto=false → silent overflow compaction is not triggered
// (result.Compacted=false), behavior identical to before the change.
func TestRun_SilentUsageOverflowDisabled(t *testing.T) {
	tool := miniagent.Tool{Name: "t", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "tr"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":6000,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("done")}}
	chat, stream := testClients(tr)
	history := []miniagent.Message{{Role: miniagent.RoleUser, Content: "h1"}, {Role: miniagent.RoleUser, Content: "h2"}, {Role: miniagent.RoleUser, Content: "h3"}, {Role: miniagent.RoleUser, Content: "h4"}, {Role: miniagent.RoleUser, Content: "h5"}, {Role: miniagent.RoleUser, Content: "h6"}}
	before, after := NewCompaction(CompactionOptions{Chat: chat, ContextWindow: 10000, Auto: false, Model: "m"})
	res, err := miniagent.Run(context.Background(), &openai.Provider{Chat: chat, Stream: stream}, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: history}, "prompt", miniagent.LoopHooks{BeforeLLM: before, AfterLLM: after}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if res.Compacted {
		t.Error("CompactionAuto=false should not trigger silent overflow compaction (result.Compacted=false)")
	}
}
