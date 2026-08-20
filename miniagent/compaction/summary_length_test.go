package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// R1 summary-path defense: a length-truncated EMPTY summary (reasoning burned the budget) must surface as an error so
// FitHistory falls back to lossy compaction, instead of silently persisting a degraded summary.
func TestSummarizeMiddle_LengthEmptyReasoning_ReturnsError(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"deep thoughts that ate the output budget"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`
	tr := &fakeTransport{responses: []string{body}}
	chat, _ := testClients(tr)
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}}
	_, _, err := summarizeMiddle(context.Background(), chat, "m", "", "", "", "", "", 5000, 2500, msgs)
	if err == nil {
		t.Fatal("expected error for length-truncated empty summary, got nil")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error should mention length truncation, got: %v", err)
	}
}

// R1 summary-path control: non-empty text truncated by length is still usable — kept (TruncateHeadTail handles it),
// no error. The defense only rejects the empty/reasoning-burned case.
func TestSummarizeMiddle_LengthNonEmpty_KeepsText(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"## Goal: partial summary text cut mid-sentence\n\n## Progress: partial","reasoning_content":"thoughts"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`
	tr := &fakeTransport{responses: []string{body}}
	chat, _ := testClients(tr)
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}}
	txt, _, err := summarizeMiddle(context.Background(), chat, "m", "", "", "", "", "", 5000, 2500, msgs)
	if err != nil {
		t.Fatalf("non-empty length should NOT error, got: %v", err)
	}
	if !strings.Contains(txt, "partial summary text") {
		t.Errorf("non-empty truncated text should be kept, got: %q", txt)
	}
}
