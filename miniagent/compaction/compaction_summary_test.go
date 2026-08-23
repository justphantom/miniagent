package compaction

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/policy"
	"github.com/justphantom/miniagent/provider/openai"
)

func TestCompactWithSummary_StripsMiddleBeforeSummarize(t *testing.T) {
	var captured []miniagent.Message
	budget := ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prev string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			captured = middle
			return "summary", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "head first round"}}
	for i := range 6 {
		id := "c" + strconv.Itoa(i)
		msgs = append(msgs, miniagent.Message{
			Role:      miniagent.RoleAssistant,
			Content:   "read file",
			Reasoning: strings.Repeat("z", 800),
			ToolCalls: []miniagent.ToolCall{{ID: id, Name: "read", Args: `{"path":"/f.go","offset":1}`}},
		})
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleTool, ToolCallID: id, Content: strings.Repeat("x", 800)})
	}
	for range 4 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "recent"})
	}
	out, _, _, err := compactWithSummary(context.Background(), budget, msgs, 4)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	for _, m := range captured {
		if m.Role == miniagent.RoleAssistant && m.Reasoning != "" {
			t.Errorf("middle reasoning should be fully cleared before summarizing, still has %d runes", len([]rune(m.Reasoning)))
		}
	}
	// dedup (P6) now takes effect (windowStartOf(0)=len, fully outside window): of 6 reads with same path/offset, keep the last in time order, earlier ones become placeholders.
	deduped := 0
	for _, m := range captured {
		if m.Role == miniagent.RoleTool && strings.Contains(m.Content, "superseded by a more recent read") {
			deduped++
		}
	}
	if deduped == 0 {
		t.Errorf("expected same path/offset reads to be deduped to placeholders, but captured tool has none")
	}
	// After reasoning cleared (~2400 tokens) + read dedup (6->1), size should be < 1500.
	capturedTokens := policy.EstimateTokens(captured, "", nil)
	if capturedTokens > 1500 {
		t.Errorf("middle size after strip should be < 1500 (reasoning cleared + read dedup), actual %d", capturedTokens)
	}
	if err := miniagent.ValidateToolPairing(out); err != nil {
		t.Errorf("pairing broken after strip: %v", err)
	}
}

func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("compacted summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	var newMsgs []miniagent.Message
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if summary.Kind != miniagent.KindSummary {
		t.Fatal("expected summary.Kind == miniagent.KindSummary")
	}
	if err := miniagent.ValidateToolPairing(out); err != nil {
		t.Errorf("result pairing broken: %v", err)
	}
	// Earliest 1 round + summary + most-recent 3 rounds
	if len(out) != 1+1+3 {
		t.Errorf("out len = %d, want 5", len(out))
	}
	if out[1].Kind != miniagent.KindSummary || !strings.Contains(out[1].Content, "compacted summary") {
		t.Errorf("summary slot wrong: %+v", out[1])
	}
	newMsgs = append([]miniagent.Message{summary}, newMsgs...)
	if len(newMsgs) != 1 || newMsgs[0].Kind != miniagent.KindSummary {
		t.Errorf("summary not persisted to newMsgs: %+v", newMsgs)
	}
}

func TestCompactWithSummary_PairingBreakErrors(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleTool, ToolCallID: "orphan", Content: "x"}, // broken pairing
		{Role: miniagent.RoleUser, Content: "u2"},
		{Role: miniagent.RoleUser, Content: "u3"},
		{Role: miniagent.RoleUser, Content: "u4"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 1); err == nil {
		t.Fatal("expected pairing-break error")
	}
}

func TestCompactWithSummary_NoMiddleNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("should-not-call")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "u1"}, {Role: miniagent.RoleUser, Content: "u2"}}
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 6)
	if err != nil || summary.Kind == miniagent.KindSummary {
		t.Fatalf("expected (no-summary,nil), got (kind=%v,err=%v)", summary.Kind, err)
	}
	if len(out) != len(msgs) {
		t.Errorf("should be unchanged: out=%d", len(out))
	}
	if tr.calls != 0 {
		t.Errorf("should not call LLM without middle: calls=%d", tr.calls)
	}
}

func TestSummarizeMiddle_LLMError(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}

func TestSummarizeMiddle_ReturnsUsage(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"## Goal: usage probe\n\n## Progress: usage probe"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":30}}`
	tr := &fakeTransport{responses: []string{body}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, usage, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if usage.InputTokens != 50 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want {50,30}", usage)
	}
}

func TestSummarizeMiddle_SetsMaxTokens(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// Reference constant rather than magic number: summaryMaxTokens now derived from summaryMaxChars/2, auto-follows future chars changes.
	if !strings.Contains(tr.lastBody, `"max_tokens":`+strconv.Itoa(summaryMaxTokens)) {
		t.Errorf("summary request did not set max_tokens=%d: %s", summaryMaxTokens, tr.lastBody)
	}
}

func TestCompactWithSummary_CompactionModelOverride(t *testing.T) {
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{summaryResponse("x")}}}}
	var gotModel string
	budget := ContextBudget{
		Model:           "main-model",
		CompactionModel: "compaction-model",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotModel = model
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, "", "", "", summaryMaxChars, 0, middle)
		},
	}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 3); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotModel != "compaction-model" {
		t.Errorf("Summarize model = %q, want compaction-model", gotModel)
	}
}

func TestBuildSummarizerSystem_CreateMode(t *testing.T) {
	got := buildSummarizerSystem("", "", "", "", "", 5000)
	for _, want := range []string{"## Goal", "## Key Details", "## Progress", "## Next Step", "## Relevant Files"} {
		if !strings.Contains(got, want) {
			t.Errorf("CREATE mode should contain template section %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<previous-summary>") {
		t.Errorf("CREATE mode should not contain <previous-summary>: %s", got)
	}
}

func TestBuildSummarizerSystem_UpdateMode(t *testing.T) {
	got := buildSummarizerSystem("", "old-anchor", "", "", "", 5000)
	for _, want := range []string{"<previous-summary>\nold-anchor\n</previous-summary>", "update the existing anchored summary", "## Goal", "## Relevant Files"} {
		if !strings.Contains(got, want) {
			t.Errorf("UPDATE mode should contain %q:\n%s", want, got)
		}
	}
}

func TestBuildSummarizerSystem_Override(t *testing.T) {
	got := buildSummarizerSystem("custom{max_chars}", "old", "", "", "", 5000)
	if !strings.HasPrefix(got, "custom5000") {
		t.Errorf("override should render {max_chars}: %q", got)
	}
	if !strings.Contains(got, "<previous-summary>") || !strings.Contains(got, "</previous-summary>") || !strings.Contains(got, "old") {
		t.Errorf("override with non-empty previousSummary should append the <previous-summary> block: %q", got)
	}
	if strings.Contains(got, "## Goal") {
		t.Errorf("override should not contain the summaryTemplate: %q", got)
	}
}

func TestStripSummaryPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{summaryPrefix + "x", "x"},
		{"x", "x"},
		{"", ""},
		{summaryPrefix, ""},
	}
	for i, c := range cases {
		if got := stripSummaryPrefix(c.in); got != c.want {
			t.Errorf("case %d: stripSummaryPrefix(%q) = %q, want %q", i, c.in, got, c.want)
		}
	}
}

func TestCompactWithSummary_UpdateModeExtractsPrevSummary(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model: "m",
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
		t.Errorf("previousSummary = %q, want old-sum", gotPrev)
	}
	for _, m := range gotMiddle {
		if m.Kind == miniagent.KindSummary {
			t.Errorf("default path middle should not contain miniagent.KindSummary (old summary should be passed down as prevSummary): %+v", gotMiddle)
		}
	}
	// head set to empty: out = summaryMsg + tail (3), first is new summary.
	if len(out) != 1+3 || out[0].Kind != miniagent.KindSummary {
		t.Errorf("out should be summary+tail (head set to empty): %+v", out)
	}
}

func TestSummarizeMiddle_UpdateModeRequest(t *testing.T) {
	tr := &fakeTransport{responses: []string{summaryResponse("updated-summary")}}
	llm := &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "old-anchor", "", "", "", summaryMaxChars, 0, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// lastBody is JSON-marshaled request body, < > are escaped to &lt; &gt;; assert with unescaped tag name + old-anchor text.
	if !strings.Contains(tr.lastBody, "previous-summary") || !strings.Contains(tr.lastBody, "old-anchor") {
		t.Errorf("UPDATE mode request should contain previous-summary block + old-anchor: %s", tr.lastBody)
	}
}
