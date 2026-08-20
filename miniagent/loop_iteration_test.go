package miniagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/text"
)

// Tool calls never stop: every response returns tool_calls, hitting the maxIterations cap.
// The termination signal is expressed via Finish=max_iterations (Steps=maxIterations + empty Text).
func TestRun_MaxIterationsReturnsBurnedUsage(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error on max iterations, got %v", err)
	}
	if res.Steps != maxIterations {
		t.Errorf("Steps = %d, want %d", res.Steps, maxIterations)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty (truncated)", res.Text)
	}
	// Each step's toolResponse has prompt_tokens=1; maxIterations steps of the main loop + summary step →
	// InputTokens is at least maxIterations. A weak assertion that only checks non-zero would miss steps that
	// go unrecorded (e.g. a regression that records only 1 token would still pass green), so we check a
	// cumulative lower bound.
	if res.Usage.InputTokens < maxIterations {
		t.Errorf("usage accumulation missed: InputTokens=%d, want >= %d", res.Usage.InputTokens, maxIterations)
	}
}

// MaxIterations can override the default cap: set to 3 and the 3rd step hits the cap (unaffected by the package default of 20).
func TestRun_MaxIterationsOverride(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	responses := make([]string, 10)
	for i := range responses {
		responses[i] = toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &fakeTransport{responses: responses}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: 3}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
	}
}

// MaxIterations<=0 falls back to the default: step 2 yields the final text, verifying it was not
// misinterpreted as a tiny cap.
func TestRun_MaxIterationsNonPositiveUsesDefault(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	for _, v := range []int{0, -1} {
		tr := &fakeTransport{responses: []string{
			toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
			textResponse("ok"),
		}}
		llm := testClients(tr)
		res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, MaxIterations: v}, "x", LoopHooks{}, nil)
		if err != nil {
			t.Fatalf("MaxIterations=%d: %v", v, err)
		}
		if res.Text != "ok" {
			t.Errorf("MaxIterations=%d: Text=%q want ok", v, res.Text)
		}
	}
}

// truncateHeadTail: head n/4 + tail 3n/4 + a middle marker; short input is not truncated; n<=0 returns as-is.
func TestTruncateHeadTail(t *testing.T) {
	s := "H" + strings.Repeat("m", 100) + "T" // head H, tail T, noise in between
	got := text.TruncateHeadTail(s, 40, "…[middle omitted]")
	if !strings.HasPrefix(got, "H") || !strings.HasSuffix(got, "T") {
		t.Errorf("head/tail chars should be preserved: %q", got)
	}
	if !strings.Contains(got, "…[middle omitted]") {
		t.Errorf("should contain the middle marker: %q", got)
	}
	// Head takes n/4=10, tail takes 30.
	if !strings.HasPrefix(got, "H"+strings.Repeat("m", 9)) {
		t.Errorf("head should occupy n/4: %q", got[:min(len(got), 12)])
	}
	// Short input is not truncated (no marker).
	if got := text.TruncateHeadTail("short", 100, "…"); strings.Contains(got, "…") {
		t.Errorf("short input should not be truncated: %q", got)
	}
	// n<=0 returns as-is.
	if got := text.TruncateHeadTail(s, 0, "…"); got != s {
		t.Errorf("n<=0 should return as-is")
	}
}

// C-3: reasoning enters the transcript and is fed back as reasoning_content in the next request.
func TestRun_ReasoningEntersHistory(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"think-step1","tool_calls":[{"id":"c1","type":"function","function":{"name":"q","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tr := &fakeTransport{responses: []string{step1, textResponse("done")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && m.Reasoning == "think-step1" {
			found = true
		}
	}
	if !found {
		t.Errorf("reasoning not in transcript: %+v", res.Messages)
	}
	if !strings.Contains(tr.lastBody, "reasoning_content") || !strings.Contains(tr.lastBody, "think-step1") {
		t.Errorf("reasoning not sent back in next request: %s", tr.lastBody)
	}
}

// StepOutput{Commit:true, View:nil} (caller misuse) must not clear the transcript —
// the Commit path of applyBeforeLLM uses toSend (already nil-rescued) rather than the raw View.
func TestApplyBeforeLLM_CommitNilViewKeepsTranscript(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "keep"}}
	newMsgs := []Message{{Role: RoleUser, Content: "keep"}}
	total := &Usage{}
	compacted := false
	hooks := LoopHooks{
		BeforeLLM: func(context.Context, StepInput) (StepOutput, error) {
			return StepOutput{Commit: true, View: nil}, nil
		},
	}
	toSend, _, err := applyBeforeLLM(context.Background(), hooks, 1, &msgs, &newMsgs, total, &compacted, LoopConfig{})
	if err != nil {
		t.Fatalf("applyBeforeLLM: %v", err)
	}
	if len(toSend) != 1 || toSend[0].Content != "keep" {
		t.Errorf("toSend should preserve the original transcript: %+v", toSend)
	}
	if len(msgs) != 1 || msgs[0].Content != "keep" {
		t.Errorf("Commit+View=nil must not clear msgs: %+v", msgs)
	}
}

// The RoleSystem bootstrap message injected by the summary path must not enter Result.Messages —
// the request uses a temporary reqMsgs, keeping the transcript clean (before the fix summaryReq did a raw
// append into msgs, leaking it).
func TestRun_SummaryPathDoesNotLeakSystemPrompt(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	for i, m := range res.Messages {
		if m.Role == RoleSystem {
			t.Errorf("Result.Messages[%d] should not contain an internal RoleSystem bootstrap message: %+v", i, m)
		}
	}
}

// When the summary step returns tool_calls (falling back to FinishMaxIterations), that call's usage should
// still be accounted for (consistent with the main path's tool_calls; before the fix the tool_calls branch
// returned before accumulation, so the summary-step tokens never entered the total).
func TestRun_SummaryFallbackAccountsUsage(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	step1 := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
	summary := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"loop","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":200,"completion_tokens":20}}`
	tr := &fakeTransport{responses: []string{step1, summary}}
	llm := testClients(tr)
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", res.Finish, FinishMaxIterations)
	}
	if res.Usage.InputTokens != 300 || res.Usage.OutputTokens != 30 {
		t.Errorf("Usage = %+v, want {300, 30} (step1 100+10 + summary 200+20)", res.Usage)
	}
}

// When the summary-step AfterLLM throws an error, Steps counts s (=iterLimit), consistent with the step-1
// semantics of a main-path AfterLLM error (recordStepUsage did not run, usage for that step is not recorded).
// Before the fix it was mistakenly recorded as s+1.
func TestRun_SummaryAfterLLMErrorSteps(t *testing.T) {
	tool := Tool{Name: "loop", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c", Name: "loop", Args: "{}"}),
		textResponse("summary"),
	}}
	llm := testClients(tr)
	hooks := LoopHooks{
		AfterLLM: func(_ context.Context, step int, _ Response) error {
			if step == 2 { // summary step s+1
				return errors.New("afterllm boom")
			}
			return nil
		},
	}
	cfg := LoopConfig{Tools: []Tool{tool}, MaxIterations: 1}
	res, err := Run(context.Background(), llm, cfg, "x", hooks, nil)
	if err == nil {
		t.Fatal("expected AfterLLM error")
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1 (summary AfterLLM err counts s=iterLimit per the usage-not-recorded semantics)", res.Steps)
	}
}
