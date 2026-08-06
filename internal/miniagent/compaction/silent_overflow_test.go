package compaction

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// §P1-B isUsageOverflow 表驱动（B.5 用例集）。
func TestIsUsageOverflow(t *testing.T) {
	cases := []struct {
		name                          string
		u                             miniagent.Usage
		contextWindow, maxTokens, res int
		auto                          bool
		want                          bool
	}{
		{"auto_off", miniagent.Usage{InputTokens: 99000, OutputTokens: 0}, 100000, 4096, 0, false, false},
		{"window_zero", miniagent.Usage{InputTokens: 99000, OutputTokens: 0}, 0, 4096, 0, true, false},
		{"under_usable", miniagent.Usage{InputTokens: 50000, OutputTokens: 0}, 100000, 4096, 0, true, false}, // usable=95904
		{"equal_usable", miniagent.Usage{InputTokens: 95904, OutputTokens: 0}, 100000, 4096, 0, true, true},  // 95904>=95904
		{"over_usable", miniagent.Usage{InputTokens: 96000, OutputTokens: 0}, 100000, 4096, 0, true, true},
		{"reserved_explicit", miniagent.Usage{InputTokens: 95500, OutputTokens: 0}, 100000, 4096, 5000, true, true}, // usable=95000
		{"maxtokens_fallback", miniagent.Usage{InputTokens: 92500, OutputTokens: 0}, 100000, 8000, 0, true, true},   // reserve=8000,usable=92000
		{"zero_usage", miniagent.Usage{InputTokens: 0, OutputTokens: 0}, 100000, 4096, 0, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUsageOverflow(c.u, c.contextWindow, c.maxTokens, c.res, c.auto); got != c.want {
				t.Errorf("isUsageOverflow = %v, want %v", got, c.want)
			}
		})
	}
}

// §P1-B compactionReserve 三分支。
func TestCompactionReserve(t *testing.T) {
	cases := []struct {
		maxTokens, res, want int
	}{
		{1000, 500, 500},  // reservedCfg>0 wins
		{8000, 0, 8000},   // min(20000,8000)
		{10000, 0, 10000}, // min(20000,10000)
		{0, 0, 20000},     // no maxTokens → buffer
		{30000, 0, 20000}, // min(20000,30000)=20000
	}
	for i, c := range cases {
		if got := compactionReserve(c.maxTokens, c.res); got != c.want {
			t.Errorf("case %d: compactionReserve(%d,%d) = %d, want %d", i, c.maxTokens, c.res, got, c.want)
		}
	}
}

// §P1-B usableTokens：window<=0→0；正常减 reserve；clamp 0。
func TestUsableTokens(t *testing.T) {
	cases := []struct {
		contextWindow, maxTokens, res, want int
	}{
		{0, 1000, 0, 0},
		{100000, 4096, 0, 95904},
		{100000, 4096, 5000, 95000},
		{100, 4096, 0, 0}, // 100-4096<0 → clamp 0
	}
	for i, c := range cases {
		if got := usableTokens(c.contextWindow, c.maxTokens, c.res); got != c.want {
			t.Errorf("case %d: usableTokens = %d, want %d", i, got, c.want)
		}
	}
}

// §P1-B usageFootprint = input+output（与 contextTokensFromUsage 同口径）。
func TestUsageFootprint(t *testing.T) {
	if got := usageFootprint(miniagent.Usage{InputTokens: 100, OutputTokens: 50}); got != 150 {
		t.Errorf("usageFootprint = %d, want 150", got)
	}
	if got := usageFootprint(miniagent.Usage{}); got != 0 {
		t.Errorf("usageFootprint zero = %d, want 0", got)
	}
}

// §P1-B Force=true 时，即使 miniagent.EstimateTokens 远低于 4/5 阈值，FitHistory 也走 compactWithSummary。
func TestFitHistory_ForceCompactsRegardlessOfEstimate(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("forced summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	budget := ContextBudget{
		ContextWindow: 1000000, // 4/5=800000：miniagent.EstimateTokens(~小) << 阈值 → 通常不压
		KeepRecent:    3,
		Force:         true,
		Summarize:     testBudget(llm).Summarize,
	}
	_, summary, summarized, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if !summarized || summary.Kind != miniagent.KindSummary {
		t.Errorf("Force=true 应触发压缩（无视 estimate）: summarized=%v kind=%v", summarized, summary.Kind)
	}
}

// §P1-B 集成：上一步真实 usage 撞窗 → 下一步强制压缩（result.Compacted=true）。
// 需要 step1 返回 tool_call（使 miniagent.Run 继续）+ 巨大 usage；step2 FitHistory(Force) 摘要（消费一个响应）；step2 最终文本。
func TestRun_SilentUsageOverflowTriggersCompaction(t *testing.T) {
	tool := miniagent.Tool{Name: "t", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "tr"} }}
	// step1：tool_call + 巨大 prompt_tokens（>= usable=5904）触发 overflow。
	step1 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":6000,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("compaction-summary"), textResponse("done")}}
	chat, stream := testClients(tr)
	// 6 轮 history + prompt → step2 压缩时有中段可摘（>1+keepRecent=5）。
	history := []miniagent.Message{{Role: miniagent.RoleUser, Content: "h1"}, {Role: miniagent.RoleUser, Content: "h2"}, {Role: miniagent.RoleUser, Content: "h3"}, {Role: miniagent.RoleUser, Content: "h4"}, {Role: miniagent.RoleUser, Content: "h5"}, {Role: miniagent.RoleUser, Content: "h6"}}
	before, after := NewCompaction(CompactionOptions{Chat: chat, ContextWindow: 10000, MaxTokens: 4096, Auto: true, Model: "m"})
	res, err := miniagent.Run(context.Background(), &openai.Provider{Chat: chat, Stream: stream}, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: history}, "prompt", miniagent.LoopHooks{BeforeLLM: before, AfterLLM: after}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if !res.Compacted {
		t.Error("静默溢出应在下一步触发压缩（result.Compacted=true）")
	}
}

// §P1-B 对照：CompactionAuto=false → 不触发静默溢出压缩（result.Compacted=false），行为同改动前。
func TestRun_SilentUsageOverflowDisabled(t *testing.T) {
	tool := miniagent.Tool{Name: "t", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "tr"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":6000,"completion_tokens":100}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("done")}}
	chat, stream := testClients(tr)
	history := []miniagent.Message{{Role: miniagent.RoleUser, Content: "h1"}, {Role: miniagent.RoleUser, Content: "h2"}, {Role: miniagent.RoleUser, Content: "h3"}, {Role: miniagent.RoleUser, Content: "h4"}, {Role: miniagent.RoleUser, Content: "h5"}, {Role: miniagent.RoleUser, Content: "h6"}}
	before, after := NewCompaction(CompactionOptions{Chat: chat, ContextWindow: 10000, MaxTokens: 4096, Auto: false, Model: "m"})
	res, err := miniagent.Run(context.Background(), &openai.Provider{Chat: chat, Stream: stream}, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: history}, "prompt", miniagent.LoopHooks{BeforeLLM: before, AfterLLM: after}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	if res.Compacted {
		t.Error("CompactionAuto=false 不应触发静默溢出压缩（result.Compacted=false）")
	}
}
