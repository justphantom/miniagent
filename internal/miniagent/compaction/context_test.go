package compaction

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/session"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// P1-1 regression: after compactWithSummary the summary must precede the user_prompt. The loop.go miniagent.Run
// entry adds this turn's user_prompt to newMsgs first, so at that point newMsgs=[user_prompt]; the merge path
// (mergePersisted in production) prepends the summary so it ranks before the user_prompt — otherwise the next
// turn's applyCompactionBarrier would barrier out this turn's user_prompt.
func TestCompactWithSummary_SummaryBeforeUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("history summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	// Simulate loop.go miniagent.Run: the entry has already added this turn's user_prompt to newMsgs and msgs.
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "current-turn-question"}}
	msgs = append(msgs, newMsgs...)
	_, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	if len(newMsgs) != 2 {
		t.Fatalf("newMsgs len=%d, want 2 (summary+user_prompt): %+v", len(newMsgs), newMsgs)
	}
	if newMsgs[0].Kind != miniagent.KindSummary {
		t.Errorf("newMsgs[0] should be summary, got %+v", newMsgs[0])
	}
	if newMsgs[1].Role != miniagent.RoleUser || newMsgs[1].Content != "current-turn-question" {
		t.Errorf("newMsgs[1] should be this turn's user_prompt, got %+v", newMsgs[1])
	}
}

// P1-1 end-to-end: compactWithSummary + summary-prepending merge (mergePersisted in production) → session.AppendMessages
// persists → session.LoadSession reads → applyCompactionBarrier: this turn's user_prompt must still be in the result.
func TestCompactWithSummary_CrossTurnBarrierPreservesUserPrompt(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("previous conversation summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "hist" + strconv.Itoa(i)})
	}
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "previous-question"}}
	msgs = append(msgs, newMsgs...)
	_, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	// Simulate the end of the previous miniagent.Run appending the assistant's final answer to newMsgs (the continuing conversation depends on the previous turn's answer).
	newMsgs = append(newMsgs, miniagent.Message{Role: miniagent.RoleAssistant, Content: "previous-answer"})

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := session.AppendMessages(path, session.SessionMeta{ID: "s"}, newMsgs); err != nil {
		t.Fatalf("session.AppendMessages: %v", err)
	}
	_, loaded, err := session.LoadSession(path)
	if err != nil {
		t.Fatalf("session.LoadSession: %v", err)
	}
	barrier := applyCompactionBarrier(loaded)
	var hasPrompt, hasAnswer bool
	for _, m := range barrier {
		if m.Content == "previous-question" {
			hasPrompt = true
		}
		if m.Content == "previous-answer" {
			hasAnswer = true
		}
	}
	if !hasPrompt {
		t.Errorf("P1-1 regression: this turn's user_prompt is missing after barrier: barrier=%+v", barrier)
	}
	if !hasAnswer {
		t.Errorf("this turn's assistant answer is missing after barrier: barrier=%+v", barrier)
	}
}

// mergeSummaryIntoNewMsgs is the test-side merge equivalent of loop.go mergePersisted: it drops any KindSummary
// already in newMsgs then prepends the summary (the cross-turn barrier-hits-the-latest-summary semantics).
func mergeSummaryIntoNewMsgs(newMsgs *[]miniagent.Message, summary miniagent.Message) {
	filtered := make([]miniagent.Message, 0, len(*newMsgs)+1)
	for _, m := range *newMsgs {
		if m.Kind != miniagent.KindSummary {
			filtered = append(filtered, m)
		}
	}
	*newMsgs = append([]miniagent.Message{summary}, filtered...)
}

// P2 single-turn multiple-compaction reversal: when compaction triggers >=2 times within a single turn, the second
// time's middle contains the old summary written by the first (which gets further compressed into the new summary).
// After the merge path (mergePersisted in production, drops the old by Kind then prepends) newMsgs has only one
// miniagent.KindSummary (the latest), ranked first, and applyCompactionBarrier hits it.
func TestCompactWithSummary_SingleTurnMultiplePreservesOrder(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("summary1"), summaryResponse("summary2")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 20 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "current-turn-question"}}
	msgs = append(msgs, newMsgs...)

	out, summary1, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary1.Kind != miniagent.KindSummary {
		t.Fatalf("1st compactWithSummary: kind=%v err=%v", summary1.Kind, err)
	}
	msgs = out
	mergeSummaryIntoNewMsgs(&newMsgs, summary1)
	// Simulate stepping: append more rounds so the window is exceeded again and a second compaction triggers (miniagent.Run's appendMsg writes both msgs and newMsgs).
	for i := range 10 {
		m := miniagent.Message{Role: miniagent.RoleUser, Content: "more" + strconv.Itoa(i)}
		msgs = append(msgs, m)
		newMsgs = append(newMsgs, m)
	}
	out2, summary2, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary2.Kind != miniagent.KindSummary {
		t.Fatalf("2nd compactWithSummary: kind=%v err=%v", summary2.Kind, err)
	}
	_ = out2
	mergeSummaryIntoNewMsgs(&newMsgs, summary2)

	count := 0
	for _, m := range newMsgs {
		if m.Kind == miniagent.KindSummary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 summary after two compactions (old must be dropped), got %d: %+v", count, newMsgs)
	}
	if newMsgs[0].Kind != miniagent.KindSummary || !strings.Contains(newMsgs[0].Content, "summary2") {
		t.Errorf("newest summary must be first: %+v", newMsgs[0])
	}
	barrier := applyCompactionBarrier(newMsgs)
	if len(barrier) == 0 || barrier[0].Kind != miniagent.KindSummary || !strings.Contains(barrier[0].Content, "summary2") {
		t.Errorf("barrier should start at newest summary: %+v", barrier)
	}
}

// P2-1 cross-turn inheritance: the old miniagent.KindSummary brought in by the previous session.LoadSession lands at
// the head of msgs via applyCompactionBarrier, and splitRounds makes it a standalone rounds[0]; after compactWithSummary
// detects it (§P0-A default path) it is extracted as previousSummary and passed down via UPDATE mode, so the old
// summary text still appears in the LLM request body (the <previous-summary> block of the system prompt), truly
// inherited rather than a broken chain.
func TestCompactWithSummary_CrossTurnInheritsLegacySummary(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("new-summary-content")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "[Previous Conversation Summary]\nlegacy-summary-content-old"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "real5"},
	}
	newMsgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "current-turn-question"}}
	msgs = append(msgs, newMsgs...)

	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	_ = newMsgs

	// (a) The LLM input must contain the old summary text — before the fix middle excluded rounds[0], breaking the chain.
	if len(tr.bodies) == 0 {
		t.Fatal("expected summarizeMiddle to be called")
	}
	if !strings.Contains(tr.bodies[0], "legacy-summary-content-old") {
		t.Errorf("LLM input should contain the old summary text to truly inherit: body=%s", tr.bodies[0])
	}
	// (b) The result has exactly 1 miniagent.KindSummary, at the head (the old summary has been merged into the new, not kept standalone).
	count := 0
	for _, m := range out {
		if m.Kind == miniagent.KindSummary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 miniagent.KindSummary in out, got %d: %+v", count, out)
	}
	if len(out) == 0 || out[0].Kind != miniagent.KindSummary || !strings.Contains(out[0].Content, "new-summary-content") {
		t.Errorf("head should be the new summary: %+v", out)
	}
	if len(out) != 1+3 {
		t.Errorf("out len = %d, want 4 (1 summary + 3 recent): %+v", len(out), out)
	}
	// (c) applyCompactionBarrier hits the new summary at the head.
	barrier := applyCompactionBarrier(out)
	if len(barrier) == 0 || barrier[0].Kind != miniagent.KindSummary || !strings.Contains(barrier[0].Content, "new-summary-content") {
		t.Errorf("barrier should start at new summary: %+v", barrier)
	}
}

// FitHistory: not over the window (ContextWindow<=0) → noop as-is.
func TestFitHistory_NoWindowNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}
	out, summary, summarized, _, usage, _, err := FitHistory(context.Background(), msgs, ContextBudget{Summarize: testBudget(llm).Summarize}, nil)
	if err != nil || summarized || summary.Kind == miniagent.KindSummary {
		t.Fatalf("expected noop, got out=%d summarized=%v kind=%v err=%v", len(out), summarized, summary.Kind, err)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("noop usage should be zero: %+v", usage)
	}
	if tr.calls != 0 {
		t.Errorf("should not call LLM: calls=%d", tr.calls)
	}
}

// FitHistory: summarization failure falls back to lossy compactHistory, and when the final result fits the window → no error, summarized=false.
func TestFitHistory_SummarizeErrorFallsBackLossy(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	big := strings.Repeat("x", 1000) // each msg ~250 tokens, making 30 msgs far exceed the window; compacted to 4 it falls back inside the window
	var msgs []miniagent.Message
	for range 30 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: big})
	}
	// ContextWindow=4000 → threshold 3200: 30 msgs (~7900) exceed the window and trigger; compactHistory compacts to 4 msgs (~1400) which falls back inside the window.
	budget := ContextBudget{ContextWindow: 4000, KeepRecent: 3, Summarize: testBudget(llm).Summarize}
	out, _, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("lossy fallback should not error when it fits: %v", err)
	}
	if summarized {
		t.Error("summarize failed → summarized should be false")
	}
	if len(out) >= len(msgs) {
		t.Errorf("lossy compaction should shrink msgs: out=%d in=%d", len(out), len(msgs))
	}
}

// L3-6: FitHistory is an exported function; if a direct caller forgets to set Summarize (Force=true or over the
// window entering compactWithSummary) → the nil guard returns an error → FitHistory falls back to lossy
// compactHistory (no panic, summarized=false, no error).
func TestFitHistory_NilSummarizeFallsBackLossy(t *testing.T) {
	big := strings.Repeat("x", 1000) // each msg ~250 tokens
	var msgs []miniagent.Message
	for range 30 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: big})
	}
	// Force=true skips the 4/5 gate and enters compactWithSummary directly; Summarize=nil (zero value) triggers the nil guard → lossy fallback.
	budget := ContextBudget{ContextWindow: 4000, KeepRecent: 3, Force: true}
	out, _, summarized, _, _, _, err := FitHistory(context.Background(), msgs, budget, nil)
	if err != nil {
		t.Fatalf("nil Summarize should fall back to lossy rather than error: %v", err)
	}
	if summarized {
		t.Error("Summarize=nil → summarized should be false (lossy fallback)")
	}
	if len(out) >= len(msgs) {
		t.Errorf("lossy fallback should shrink: out=%d in=%d", len(out), len(msgs))
	}
}
