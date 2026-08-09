package policy

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// TrimForHistory: an explicit limit trims down to that value; limit<=0 uses the default MaxToolResultInHistory. split=false uses head-only.
func TestTrimForHistory_PerLimit(t *testing.T) {
	big := strings.Repeat("x", 10000)
	got := TrimForHistory(big, 8000, false)
	if len(got) <= 8000 || !strings.Contains(got, "truncated") {
		t.Errorf("limit=8000: len=%d, marker missing: %q", len(got), got[:min(len(got), 40)])
	}
	got0 := TrimForHistory(big, 0, false)
	// Default trims down to MaxToolResultInHistory: length should be slightly above that value (marker included), well below 8000.
	if len(got0) <= miniagent.MaxToolResultInHistory || len(got0) >= 8000 {
		t.Errorf("limit=0: len=%d, want in (%d, 8000)", len(got0), miniagent.MaxToolResultInHistory)
	}
}

// TrimForHistory split=true (shell/grep): head+tail segmented truncation, retaining the tail error conclusion marker,
// total length near limit.
func TestTrimForHistory_SplitKeepsTail(t *testing.T) {
	// Head context + a large middle noise section + tail error conclusion. head-only would drop the FAIL line.
	body := "CMD: build\n" + strings.Repeat("log\n", 2000) + "FAIL: exit status 1"
	got := TrimForHistory(body, 4000, true)
	if !strings.Contains(got, "middle omitted") {
		t.Errorf("split should contain the middle-omitted marker: %q", got[:min(len(got), 60)])
	}
	if !strings.Contains(got, "FAIL: exit status 1") {
		t.Errorf("split should retain the tail error conclusion: tail=%q", got[max(0, len(got)-80):])
	}
	if !strings.Contains(got, "CMD: build") {
		t.Errorf("split should retain the head context: head=%q", got[:min(len(got), 40)])
	}
	if len(got) > 4200 { // head n/4 + tail 3n/4 + marker, should slightly exceed limit
		t.Errorf("split total length should be near limit: len=%d", len(got))
	}
}
