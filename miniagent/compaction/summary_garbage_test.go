package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// P0 corrupted-summary guard: a summarizer that echoes tool-call markup (the prompt-injection incident: a <tool_call> python
// draft persisted as KindSummary and replayed on resume) must be rejected, retried ONCE with the prose-only directive, and the
// retry's clean output persisted. Both calls' usage is accumulated.
func TestSummarizeMiddle_ToolCallOutput_RetriedOnceAndKept(t *testing.T) {
	garbage := `{"choices":[{"message":{"role":"assistant","content":"<tool_call>python rewrite script</tool_call>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
	clean := `{"choices":[{"message":{"role":"assistant","content":"## Goal: rewrite\n\n## Progress: rewrote the summary pipeline"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":20}}`
	tr := &fakeTransport{responses: []string{garbage, clean}}
	chat, _ := testClients(tr)
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}}
	txt, usage, err := summarizeMiddle(context.Background(), chat, "m", "", "", "", "", "", 5000, 2500, msgs)
	if err != nil {
		t.Fatalf("retry with clean prose should succeed, got: %v", err)
	}
	if !strings.Contains(txt, "## Goal") {
		t.Errorf("clean retry output should be kept, got: %q", txt)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (initial + one retry)", tr.calls)
	}
	if len(tr.bodies) == 2 && !strings.Contains(tr.bodies[1], "OUTPUT PROSE ONLY") {
		t.Errorf("retry request should carry the prose-only directive in system, got: %s", tr.bodies[1])
	}
	if usage.InputTokens != 22 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want accumulated {22,30}", usage)
	}
}

// P0 corrupted-summary guard: garbage on BOTH attempts surfaces as an error so FitHistory falls back to lossy compaction —
// the garbage is never persisted as KindSummary.
func TestSummarizeMiddle_ToolCallOutputTwice_ReturnsError(t *testing.T) {
	garbage := `{"choices":[{"message":{"role":"assistant","content":"<tool_call>draft</tool_call>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`
	tr := &fakeTransport{responses: []string{garbage, garbage}}
	chat, _ := testClients(tr)
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}}
	_, _, err := summarizeMiddle(context.Background(), chat, "m", "", "", "", "", "", 5000, 2500, msgs)
	if err == nil {
		t.Fatal("expected error for persistent garbage output, got nil")
	}
	if !strings.Contains(err.Error(), "not prose") {
		t.Errorf("error should mention prose rejection, got: %v", err)
	}
	if tr.calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (no unbounded retry)", tr.calls)
	}
}

// Default-shape guard: a non-empty but template-less output (no >=2 sections) on the BUILT-IN summarizer shape is rejected;
// a custom summarizer prompt opts out of the section check (it may bake in its own structure) — only markup rejection applies.
func TestIsSummaryGarbage_SectionAndCustomPromptRules(t *testing.T) {
	cases := []struct {
		name                                             string
		text, prompt, createInstr, updateInstr, template string
		want                                             bool
	}{
		{"tool-call markup rejected", "x <tool_call> y", "", "", "", "", true},
		{"leading code fence rejected", "```go\nf()\n```", "", "", "", "", true},
		{"leading fence with leading blanks rejected", "\n\n```\ncmd\n```", "", "", "", "", true},
		{"in-body code fence passes", "## Goal: a\n\nran:\n```sh\ncmd\n```\n\n## Progress: b", "", "", "", "", false},
		{"template-less default rejected", "just some prose", "", "", "", "", true},
		{"two sections pass", "## Goal: a\n\n## Progress: b", "", "", "", "", false},
		{"one section rejected", "## Goal: a\n\nprose", "", "", "", "", true},
		{"exact header-only not counted", "## Goal\n\n## Progress", "", "", "", "", true},
		{"custom prompt skips section check", "freeform summary", "custom sys", "", "", "", false},
		{"custom template skips section check", "freeform summary", "", "", "", "custom template", false},
		{"custom instruction skips section check", "freeform summary", "", "do it this way", "", "", false},
		{"custom prompt still rejects markup", "has <tool_call>", "custom sys", "", "", "", true},
	}
	for _, c := range cases {
		if got := isSummaryGarbage(c.text, c.prompt, c.createInstr, c.updateInstr, c.template); got != c.want {
			t.Errorf("%s: isSummaryGarbage = %v, want %v", c.name, got, c.want)
		}
	}
}
