package compaction

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent/session"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/openai"
)

// uPtr is a small helper to construct *miniagent.Usage (avoids writing a temp variable to take its address everywhere).
func uPtr(in, out int) *miniagent.Usage { return &miniagent.Usage{InputTokens: in, OutputTokens: out} }

// §P0-B lastApplicableUsageIndex table-driven: covers the 8 cases of B.5 (including anti-staleness core + double-compaction guard).
func TestLastApplicableUsageIndex(t *testing.T) {
	asst := func(ts int64, u *miniagent.Usage) miniagent.Message {
		return miniagent.Message{Role: miniagent.RoleAssistant, Ts: ts, Usage: u}
	}
	cases := []struct {
		name string
		msgs []miniagent.Message
		want int
	}{
		{"empty", nil, -1},
		{"only_user", []miniagent.Message{{Role: miniagent.RoleUser, Ts: 1}}, -1},
		{"assistant_no_usage", []miniagent.Message{asst(1, nil)}, -1},
		{"assistant_zero_usage", []miniagent.Message{asst(1, uPtr(0, 0))}, -1},
		{"single_anchor", []miniagent.Message{asst(1, uPtr(100, 50))}, 0},
		{"last_of_two", []miniagent.Message{asst(1, uPtr(100, 50)), {Role: miniagent.RoleUser, Ts: 2}, asst(3, uPtr(200, 80))}, 2},
		{
			// Anti-staleness core: summary(Ts=3) invalidates the assistant(Ts=2) at idx1; idx3(Ts=4) is newer than the summary and becomes applicable again.
			"summary_invalidates_then_refreshed",
			[]miniagent.Message{{Role: miniagent.RoleUser, Ts: 1}, asst(2, uPtr(9000, 100)), {Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Ts: 3}, asst(4, uPtr(200, 80))},
			3,
		},
		{
			// Double-compaction guard: after the summary there is no new usage (only tool), everything is stale -> fallback.
			"summary_no_new_usage",
			[]miniagent.Message{{Role: miniagent.RoleUser, Ts: 1}, asst(2, uPtr(9000, 100)), {Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Ts: 3}, {Role: miniagent.RoleTool, ToolCallID: "x", Ts: 3}},
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

// §P0-B estimateTokensFromUsage: no anchor -> ok=false; tokens at the end of the anchor = Input+Output; anchor+trailing includes CJK.
func TestEstimateTokensFromUsage(t *testing.T) {
	if _, ok := estimateTokensFromUsage([]miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}}); ok {
		t.Error("no assistant usage should give ok=false")
	}
	// The anchor is at the end, no trailing -> tokens = Input+Output.
	tokens, ok := estimateTokensFromUsage([]miniagent.Message{
		{Role: miniagent.RoleUser, Content: "ignored"},
		{Role: miniagent.RoleAssistant, Ts: 1, Usage: uPtr(1000, 200)},
	})
	if !ok || tokens != 1200 {
		t.Errorf("anchor at end: tokens=%d ok=%v, want 1200/true", tokens, ok)
	}
	// Anchor + trailing: tokens = Input+Output + local estimate of trailing. "中文测试"=4 CJK -> 4/2=2.
	tokens, ok = estimateTokensFromUsage([]miniagent.Message{
		{Role: miniagent.RoleAssistant, Ts: 1, Usage: uPtr(500, 100)},
		{Role: miniagent.RoleUser, Content: "中文测试"},
	})
	if !ok {
		t.Error("anchor+trailing should give ok=true")
	}
	if tokens != 606 { // 600 + 2(CJK) + 4(trailing msg envelope, bug 5 adds envelope margin)
		t.Errorf("anchor+trailing(CJK): tokens=%d, want 606", tokens)
	}
}

// §P0-B estimateThreshold: no usage or kill-switch=false falls back to policy.EstimateTokens; with usage and the switch on, uses the real value.
func TestEstimateThreshold_Fallback(t *testing.T) {
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "hello world"}}
	want := miniagent.EstimateTokens(msgs, "sys", nil)
	if got := estimateThreshold(msgs, "sys", nil, true); got != want {
		t.Errorf("no usage fallback: estimateThreshold=%d, want policy.EstimateTokens=%d", got, want)
	}
	// kill-switch=false also falls back to policy.EstimateTokens (even with usage).
	msgs2 := []miniagent.Message{{Role: miniagent.RoleAssistant, Ts: 1, Usage: uPtr(100, 50)}}
	want2 := miniagent.EstimateTokens(msgs2, "sys", nil)
	if got := estimateThreshold(msgs2, "sys", nil, false); got != want2 {
		t.Errorf("kill-switch=false: estimateThreshold=%d, want policy.EstimateTokens=%d", got, want2)
	}
	if got := estimateThreshold(msgs2, "sys", nil, true); got != 150 {
		t.Errorf("kill-switch=true with usage: estimateThreshold=%d, want 150 (100+50)", got)
	}
}

// §P0-B session round-trip: an assistant line with miniagent.Usage+Ts written to jsonl is fully restored by session.LoadSession;
// an old fixture missing those fields can still be loaded with miniagent.Usage==nil.
func TestSessionRoundTrip_UsageAndTs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "q"},
		{Role: miniagent.RoleAssistant, Content: "a", Usage: uPtr(123, 45), Ts: 999},
	}
	if err := session.AppendMessages(path, session.SessionMeta{ID: "s"}, msgs); err != nil {
		t.Fatalf("session.AppendMessages: %v", err)
	}
	_, loaded, err := session.LoadSession(path)
	if err != nil {
		t.Fatalf("session.LoadSession: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded len=%d, want 2", len(loaded))
	}
	if loaded[1].Usage == nil || loaded[1].Usage.InputTokens != 123 || loaded[1].Usage.OutputTokens != 45 {
		t.Errorf("miniagent.Usage not restored: %+v", loaded[1].Usage)
	}
	if loaded[1].Ts != 999 {
		t.Errorf("Ts not restored: got %d, want 999", loaded[1].Ts)
	}

	// Old fixture (no usage/ts fields) can still be loaded, miniagent.Usage==nil, Ts==0.
	path2 := filepath.Join(t.TempDir(), "old.jsonl")
	old := `{"type":"session","id":"old"}
{"type":"message","role":"user","content":"hi"}
{"type":"message","role":"assistant","content":"yo"}`
	if err := os.WriteFile(path2, []byte(old), 0o600); err != nil {
		t.Fatalf("write old fixture: %v", err)
	}
	_, loaded2, err := session.LoadSession(path2)
	if err != nil {
		t.Fatalf("old fixture session.LoadSession: %v", err)
	}
	for i, m := range loaded2 {
		if m.Usage != nil {
			t.Errorf("old fixture msg %d should have miniagent.Usage==nil: %+v", i, m.Usage)
		}
	}
}

// §P0-B integration: when the local estimate exceeds the window but the trailing assistant's real usage
// does not exceed it, FitHistory (UseRealUsage=true) does not trigger summary compaction (Summarize is not called),
// applyContextStrips returns early -- covering the blind spot where policy.EstimateTokens has zero awareness of caching.
func TestFitHistory_RealUsagePreventsCompaction(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("should not be called")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	// Huge user content makes the local policy.EstimateTokens far exceed the window; the trailing assistant's real usage is only 150 (not over the window).
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: strings.Repeat("x", 8000)}, // ~2000 tokens local estimate
		{Role: miniagent.RoleAssistant, Ts: 1, Usage: uPtr(100, 50), Content: "a"},
	}
	budget := ContextBudget{
		ContextWindow: 1000, // 4/5=800: local ~2000 over window, real 150 not over
		UseRealUsage:  true,
		Summarize:     testBudget(llm).Summarize,
	}
	_ = tr
	out, _, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("FitHistory: %v", err)
	}
	if summarized {
		t.Error("real usage not over the window should not trigger summary compaction (summarized=true)")
	}
	if tr.calls != 0 {
		t.Errorf("Summarize should not be called: calls=%d", tr.calls)
	}
	// Contrast: kill-switch=false (falls back to local estimate) judges it over the window and enters
	// compactWithSummary (2 rounds <= 1+keepRecent, no middle noop), but after trimRecentRounds it is still over
	// -> returns an error. Here only verifies the true path does not over-compress.
	_ = out
}

// §P0-B anti-staleness/double-compaction guard (proposal B.5 case 5): after the first summary, the summaryMsg
// carries a new Ts that invalidates the old assistant usage, so the second FitHistory does not compress again
// due to the stale large usage. Guards the compactWithSummary summaryMsg Ts:text.NowMs() trigger point
// (review Finding 3: removing that Ts would cause the second round to summarize again using stale usage, failing this test).
func TestFitHistory_NoDoubleCompactionAfterSummary(t *testing.T) {
	calls := 0
	budget := ContextBudget{
		ContextWindow: 2000, // 4/5=1600; preserveRecentTokens=floor(2000/4)=500 -> clamp 2000
		KeepRecent:    4,
		UseRealUsage:  true,
		Summarize: func(_ context.Context, _, _, _ string, _ []miniagent.Message) (string, miniagent.Usage, error) {
			calls++
			return "summary", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "u0"},
		{Role: miniagent.RoleUser, Content: "u1"},
		{Role: miniagent.RoleUser, Content: "u2"},
		{Role: miniagent.RoleUser, Content: "u3"},
		{Role: miniagent.RoleUser, Content: "u4"},
		{Role: miniagent.RoleUser, Content: "u5"},
		{Role: miniagent.RoleAssistant, Content: "recent", Ts: 100, Usage: uPtr(9000, 100)}, // stale large usage
	}
	// First pass: stale large usage (9100) exceeds the 1600 threshold -> summarize.
	out1, _, summarized1, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("1st FitHistory: %v", err)
	}
	if !summarized1 {
		t.Fatal("1st pass should summarize (stale large usage over threshold)")
	}
	// Simulate the next step: after out1 (which contains the summary with a new Ts), append rounds so the second pass has enough rounds to summarize.
	msgs2 := append([]miniagent.Message{}, out1...)
	msgs2 = append(msgs2, miniagent.Message{Role: miniagent.RoleUser, Content: "u6"}, miniagent.Message{Role: miniagent.RoleUser, Content: "u7"})
	_, _, summarized2, _, _, _, err := FitHistory(context.Background(), msgs2, budget, nil)
	if err != nil {
		t.Fatalf("2nd FitHistory: %v", err)
	}
	if summarized2 {
		t.Error("anti-staleness: the summary's new Ts should invalidate the old usage, the second pass should not summarize again")
	}
	if calls != 1 {
		t.Errorf("Summarize should be called only once (double-compaction guard), got calls=%d", calls)
	}
}
