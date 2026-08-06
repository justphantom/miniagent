package miniagent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uPtr 是构造 *Usage 的小工具（避免每处都写零时变量取址）。
func uPtr(in, out int) *Usage { return &Usage{InputTokens: in, OutputTokens: out} }

// §P0-B lastApplicableUsageIndex 表驱动：覆盖 B.5 的 8 个用例（含防陈旧核心 + 二次压缩防护）。
func TestLastApplicableUsageIndex(t *testing.T) {
	asst := func(ts int64, u *Usage) Message { return Message{Role: RoleAssistant, Ts: ts, Usage: u} }
	cases := []struct {
		name string
		msgs []Message
		want int
	}{
		{"empty", nil, -1},
		{"only_user", []Message{{Role: RoleUser, Ts: 1}}, -1},
		{"assistant_no_usage", []Message{asst(1, nil)}, -1},
		{"assistant_zero_usage", []Message{asst(1, uPtr(0, 0))}, -1},
		{"single_anchor", []Message{asst(1, uPtr(100, 50))}, 0},
		{"last_of_two", []Message{asst(1, uPtr(100, 50)), {Role: RoleUser, Ts: 2}, asst(3, uPtr(200, 80))}, 2},
		{
			// 防陈旧核心：summary(Ts=3) 使 idx1 的 assistant(Ts=2) 失效，idx3(Ts=4) 新于 summary 重新可用。
			"summary_invalidates_then_refreshed",
			[]Message{{Role: RoleUser, Ts: 1}, asst(2, uPtr(9000, 100)), {Role: RoleUser, Kind: KindSummary, Ts: 3}, asst(4, uPtr(200, 80))},
			3,
		},
		{
			// 二次压缩防护：summary 后无新 usage（仅 tool），全陈旧 → 回落。
			"summary_no_new_usage",
			[]Message{{Role: RoleUser, Ts: 1}, asst(2, uPtr(9000, 100)), {Role: RoleUser, Kind: KindSummary, Ts: 3}, {Role: RoleTool, ToolCallID: "x", Ts: 3}},
			-1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastApplicableUsageIndex(c.msgs); got != c.want {
				t.Errorf("lastApplicableUsageIndex = %d, want %d", got, c.want)
			}
		})
	}
}

// §P0-B estimateTokensFromUsage：无锚点 ok=false；锚点末尾 tokens=Input+Output；锚点+trailing 含 CJK。
func TestEstimateTokensFromUsage(t *testing.T) {
	if _, ok := estimateTokensFromUsage([]Message{{Role: RoleUser, Content: "x"}}); ok {
		t.Error("无 assistant usage 应 ok=false")
	}
	// 锚点在末尾，无 trailing → tokens = Input+Output。
	tokens, ok := estimateTokensFromUsage([]Message{
		{Role: RoleUser, Content: "ignored"},
		{Role: RoleAssistant, Ts: 1, Usage: uPtr(1000, 200)},
	})
	if !ok || tokens != 1200 {
		t.Errorf("锚点末尾: tokens=%d ok=%v, want 1200/true", tokens, ok)
	}
	// 锚点 + trailing：tokens = Input+Output + trailing 本地估算。"中文测试"=4 CJK → 4/2=2。
	tokens, ok = estimateTokensFromUsage([]Message{
		{Role: RoleAssistant, Ts: 1, Usage: uPtr(500, 100)},
		{Role: RoleUser, Content: "中文测试"},
	})
	if !ok {
		t.Error("锚点+trailing 应 ok=true")
	}
	if tokens != 602 { // 600 + 2
		t.Errorf("锚点+trailing(CJK): tokens=%d, want 602", tokens)
	}
}

// §P0-B estimateThreshold：无 usage 或 kill-switch=false 回落 estimateTokens；有 usage 且开关开用真实值。
func TestEstimateThreshold_Fallback(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "hello world"}}
	want := estimateTokens(msgs, "sys", nil)
	if got := estimateThreshold(msgs, "sys", nil, true); got != want {
		t.Errorf("无 usage 回落: estimateThreshold=%d, want estimateTokens=%d", got, want)
	}
	// kill-switch=false 也回落 estimateTokens（即使有 usage）。
	msgs2 := []Message{{Role: RoleAssistant, Ts: 1, Usage: uPtr(100, 50)}}
	want2 := estimateTokens(msgs2, "sys", nil)
	if got := estimateThreshold(msgs2, "sys", nil, false); got != want2 {
		t.Errorf("kill-switch=false: estimateThreshold=%d, want estimateTokens=%d", got, want2)
	}
	if got := estimateThreshold(msgs2, "sys", nil, true); got != 150 {
		t.Errorf("kill-switch=true 有 usage: estimateThreshold=%d, want 150 (100+50)", got)
	}
}

// §P0-B appendMsg 打戳：Ts==0 自动打戳（>0）；显式 Ts 保留。
func TestAppendMsg_Timestamp(t *testing.T) {
	var msgs, newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: "auto"})
	if msgs[0].Ts == 0 {
		t.Error("Ts==0 应被 appendMsg 自动打戳为 >0")
	}
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: "manual", Ts: 42})
	if msgs[1].Ts != 42 {
		t.Errorf("显式 Ts 应保留: got %d, want 42", msgs[1].Ts)
	}
}

// §P0-B session 往返：带 Usage+Ts 的 assistant 行写 jsonl 再 LoadSession 字段完整还原；
// 缺字段的旧 fixture 仍能加载且 Usage==nil。
func TestSessionRoundTrip_UsageAndTs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	msgs := []Message{
		{Role: RoleUser, Content: "q"},
		{Role: RoleAssistant, Content: "a", Usage: uPtr(123, 45), Ts: 999},
	}
	if err := AppendMessages(path, SessionMeta{ID: "s"}, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded len=%d, want 2", len(loaded))
	}
	if loaded[1].Usage == nil || loaded[1].Usage.InputTokens != 123 || loaded[1].Usage.OutputTokens != 45 {
		t.Errorf("Usage 未还原: %+v", loaded[1].Usage)
	}
	if loaded[1].Ts != 999 {
		t.Errorf("Ts 未还原: got %d, want 999", loaded[1].Ts)
	}

	// 旧 fixture（无 usage/ts 字段）仍能加载，Usage==nil、Ts==0。
	path2 := filepath.Join(t.TempDir(), "old.jsonl")
	old := `{"type":"session","id":"old"}
{"type":"message","role":"user","content":"hi"}
{"type":"message","role":"assistant","content":"yo"}`
	if err := os.WriteFile(path2, []byte(old), 0o600); err != nil {
		t.Fatalf("write old fixture: %v", err)
	}
	_, loaded2, err := LoadSession(path2)
	if err != nil {
		t.Fatalf("旧 fixture LoadSession: %v", err)
	}
	for i, m := range loaded2 {
		if m.Usage != nil {
			t.Errorf("旧 fixture msg %d 应 Usage==nil: %+v", i, m.Usage)
		}
	}
}

// §P0-B 集成：本地估算超窗但末尾 assistant 真实 usage 未超窗时，FitHistory（UseRealUsage=true）
// 不触发摘要压缩（Summarize 不被调），applyContextStrips 早返——补 estimateTokens 对缓存零感知盲区。
func TestFitHistory_RealUsagePreventsCompaction(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("不应被调")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	// user 巨大内容使本地 estimateTokens 远超窗；末尾 assistant 真实 usage 仅 150（未超窗）。
	msgs := []Message{
		{Role: RoleUser, Content: strings.Repeat("x", 8000)}, // ~2000 token 本地估算
		{Role: RoleAssistant, Ts: 1, Usage: uPtr(100, 50), Content: "a"},
	}
	budget := ContextBudget{
		ContextWindow: 1000, // 4/5=800：本地 ~2000 超窗、真实 150 未超
		UseRealUsage:  true,
		Summarize:     testBudget(llm).Summarize,
	}
	_ = tr
	out, _, summarized, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if summarized {
		t.Error("真实 usage 未超窗时不应触发摘要压缩（summarized=true）")
	}
	if tr.calls != 0 {
		t.Errorf("Summarize 不应被调用: calls=%d", tr.calls)
	}
	// 对照：kill-switch=false（回落本地估算）会判超窗进入 compactWithSummary（2 轮 <= 1+keepRecent，
	// 无中段 noop），但 trimRecentRounds 后仍超 → 返回 error。这里只验证 true 路径不误压。
	_ = out
}

// §P0-B 防陈旧/二次压缩防护（提案 B.5 用例5）：第一次摘要后 summaryMsg 带新 Ts 使旧 assistant usage 失效，
// 第二次 FitHistory 不再因陈旧大 usage 二次压缩。守护 compactWithSummary 的 summaryMsg Ts:nowMs() 触发点
// （review Finding 3：移除该 Ts 会使第二轮用陈旧 usage 再次摘要，此测试失败）。
func TestFitHistory_NoDoubleCompactionAfterSummary(t *testing.T) {
	calls := 0
	budget := ContextBudget{
		ContextWindow: 2000, // 4/5=1600；preserveRecentTokens=floor(2000/4)=500→clamp 2000
		KeepRecent:    4,
		UseRealUsage:  true,
		Summarize: func(_ context.Context, _, _, _ string, _ []Message) (string, Usage, error) {
			calls++
			return "summary", Usage{}, nil
		},
	}
	msgs := []Message{
		{Role: RoleUser, Content: "u0"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleUser, Content: "u3"},
		{Role: RoleUser, Content: "u4"},
		{Role: RoleUser, Content: "u5"},
		{Role: RoleAssistant, Content: "recent", Ts: 100, Usage: uPtr(9000, 100)}, // 陈旧大 usage
	}
	// 第一次：陈旧大 usage（9100）超 1600 阈值 → 摘要。
	out1, _, summarized1, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("1st FitHistory: %v", err)
	}
	if !summarized1 {
		t.Fatal("1st 应摘要（陈旧大 usage 超阈值）")
	}
	// 模拟下一步：在 out1（含带新 Ts 的 summary）后追加轮次，使第二轮有足够轮次可摘要。
	msgs2 := append([]Message{}, out1...)
	msgs2 = append(msgs2, Message{Role: RoleUser, Content: "u6"}, Message{Role: RoleUser, Content: "u7"})
	_, _, summarized2, _, err := FitHistory(context.Background(), msgs2, budget, nil)
	if err != nil {
		t.Fatalf("2nd FitHistory: %v", err)
	}
	if summarized2 {
		t.Error("防陈旧：summary 新 Ts 应使旧 usage 失效，第二轮不应再次摘要")
	}
	if calls != 1 {
		t.Errorf("Summarize 应只调一次（防二次压缩），got calls=%d", calls)
	}
}
