package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// ITEM 1 (override-prompt-reverts-to-dilution, C1 residual): the override path (budget.SummarizerPrompt != "") must be
// a strict superset of the default path rather than regressing to dilution. buildSummarizerSystem now consumes
// previousSummary (placeholder substitution, or an appended <previous-summary> block), and compactWithSummary no longer
// merges the old summary into middle — it extracts it as previousSummary in BOTH branches.

// (a) override prompt WITH the {previous_summary} placeholder → the old summary is substituted inline; the separate
// <previous-summary> block is NOT appended. {max_chars} is still rendered.
func TestBuildSummarizerSystem_OverrideSubstitutesPlaceholder(t *testing.T) {
	got := buildSummarizerSystem("custom{max_chars} anchor={previous_summary}", "old-sum", "", "", "", 5000)
	if !strings.Contains(got, "custom5000 anchor=old-sum") {
		t.Errorf("override with placeholder should substitute {previous_summary} inline:\n%s", got)
	}
	if strings.Contains(got, "<previous-summary>") {
		t.Errorf("placeholder present → should NOT also append a <previous-summary> block:\n%s", got)
	}
}

// (b) override prompt WITHOUT the placeholder → the previous-summary block is appended unconditionally (backward-compat:
// no longer loses the old summary, which was the C1 regression); {max_chars} is still rendered; the summaryTemplate is
// NOT appended (too invasive — a custom prompt may bake in its own template).
func TestBuildSummarizerSystem_OverrideAppendsBlockWithoutPlaceholder(t *testing.T) {
	got := buildSummarizerSystem("custom{max_chars}", "old-sum", "", "", "", 5000)
	if !strings.HasPrefix(got, "custom5000") {
		t.Errorf("override should still render {max_chars} at the head:\n%s", got)
	}
	if !strings.Contains(got, "<previous-summary>\nold-sum\n</previous-summary>") {
		t.Errorf("override without placeholder should append the <previous-summary> block (was: old summary lost entirely):\n%s", got)
	}
	if strings.Contains(got, "## Goal") {
		t.Errorf("override should NOT append the summaryTemplate (too invasive):\n%s", got)
	}
}

// (b-extra) override prompt WITHOUT placeholder + empty previousSummary → no block appended (nothing to preserve,
// mirroring the default CREATE path). The render surface for "no old summary" is unchanged.
func TestBuildSummarizerSystem_OverrideNoBlockWhenPreviousSummaryEmpty(t *testing.T) {
	got := buildSummarizerSystem("custom{max_chars}", "", "", "", "", 5000)
	if got != "custom5000" {
		t.Errorf("override with empty previousSummary = %q, want custom5000", got)
	}
	if strings.Contains(got, "<previous-summary>") {
		t.Errorf("no old summary → should not append an empty block: %s", got)
	}
}

// (c) compactWithSummary override path no longer merges the old summary into middle: the Summarize callback receives
// previousSummary non-empty (the old summary extracted as the UPDATE anchor) and middle contains no KindSummary; head is
// set to nil so out = summaryMsg + tail.
func TestCompactWithSummary_OverrideExtractsPrevSummaryNotMerge(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model:            "m",
		SummarizerPrompt: "custom{max_chars}",
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
		t.Errorf("override path previousSummary = %q, want old-sum (extracted as UPDATE anchor, no longer empty)", gotPrev)
	}
	for _, m := range gotMiddle {
		if m.Kind == miniagent.KindSummary {
			t.Errorf("override path middle must not contain the old KindSummary (no longer merged into middle): %+v", gotMiddle)
		}
	}
	// head set to nil in both branches: out = summaryMsg + tail (3), first is the new summary.
	if len(out) != 1+3 || out[0].Kind != miniagent.KindSummary {
		t.Errorf("out should be summary+tail (head set to nil): %+v", out)
	}
}
