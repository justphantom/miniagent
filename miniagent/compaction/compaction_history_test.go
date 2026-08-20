package compaction

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// compactHistory is the lossy fallback when summarization fails or there is no middle segment (compaction_split.go):
// it retains "the earliest 1 round + the most recent keepRecent rounds" and drops the middle. This path has long
// been covered only indirectly by TestFitHistory_SummarizeErrorFallsBackLossy; this test directly locks the
// head+tail retention + middle-drop semantics.
func TestCompactHistory_DropsMiddleKeepsEnds(t *testing.T) {
	// 5 user+assistant rounds, keepRecent=2: should retain the earliest 1 round + the most recent 2 rounds, dropping the 2 middle rounds.
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleAssistant, Content: "first-a"},
		{Role: miniagent.RoleUser, Content: "mid1"},
		{Role: miniagent.RoleAssistant, Content: "mid1-a"},
		{Role: miniagent.RoleUser, Content: "mid2"},
		{Role: miniagent.RoleAssistant, Content: "mid2-a"},
		{Role: miniagent.RoleUser, Content: "last1"},
		{Role: miniagent.RoleAssistant, Content: "last1-a"},
		{Role: miniagent.RoleUser, Content: "last2"},
		{Role: miniagent.RoleAssistant, Content: "last2-a"},
	}
	out := compactHistory(msgs, 2)
	// Lossy: the output must be smaller than the input (middle dropped).
	if len(out) >= len(msgs) {
		t.Errorf("should drop the middle (lossy): out=%d msgs, in=%d", len(out), len(msgs))
	}
	// Retain the earliest round first.
	if len(out) == 0 || out[0].Content != "first" {
		t.Errorf("should retain the earliest round first: got %+v", out)
	}
	// Retain the most recent round last2-a.
	if len(out) == 0 || out[len(out)-1].Content != "last2-a" {
		t.Errorf("should retain the most recent round last2-a: got %+v", out[len(out)-1])
	}
	// The middle mid1/mid2 are all dropped.
	for _, m := range out {
		if strings.Contains(m.Content, "mid") {
			t.Errorf("middle should be dropped but is still present: %s", m.Content)
		}
	}
}

// Boundary: when the number of rounds <= 1+keepRecent, return as-is (no lossy trim needed).
func TestCompactHistory_NoopWhenFewRounds(t *testing.T) {
	// splitRounds makes each tool_call-less message its own round: 3 messages = 3 rounds, <= 1+keepRecent(=3), returned as-is.
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "u1"},
		{Role: miniagent.RoleAssistant, Content: "a1"},
		{Role: miniagent.RoleUser, Content: "u2"},
	}
	out := compactHistory(msgs, 2) // 3 rounds <= 1+2=3, as-is
	if len(out) != len(msgs) {
		t.Errorf("rounds <= 1+keepRecent should return as-is: out=%d, want %d", len(out), len(msgs))
	}
}
