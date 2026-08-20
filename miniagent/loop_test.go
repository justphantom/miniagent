package miniagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRun_AssistantTextPreservedWithToolCalls(t *testing.T) {
	resp := `{"choices":[{"message":{"role":"assistant","content":"let me check first","tool_calls":[{"id":"1","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tr := &fakeTransport{responses: []string{resp, textResponse("done")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{{Name: "read", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "ok"} }}}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var asst string
	found := false
	for _, m := range res.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			found, asst = true, m.Content
		}
	}
	if !found {
		t.Fatal("no assistant message with tool_calls found")
	}
	if asst != "let me check first" {
		t.Errorf("assistant Content with tool_calls = %q, want %q (resp.Text must not be lost, R4-2)", asst, "let me check first")
	}
}

func TestRun_TextOnlyReturnsImmediately(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("hello world")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", System: "be brief"}, "hi", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d", tr.calls)
	}
	if !strings.Contains(tr.lastBody, `"role":"system"`) || !strings.Contains(tr.lastBody, "be brief") {
		t.Errorf("system not sent: %s", tr.lastBody)
	}
}

func TestRun_ReActToolThenText(t *testing.T) {
	called := false
	tool := Tool{
		Name: "echo",
		Call: func(_ context.Context, args string) ToolResult {
			called = true
			return ToolResult{Output: "echoed " + args}
		},
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	var uses []string
	onToolUse := func(name, input string) error { uses = append(uses, name); return nil }
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{OnToolUse: onToolUse}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if !called {
		t.Error("tool not called")
	}
	// tool_use is notified only once (the terminal text is carried by Result).
	if len(uses) != 1 || uses[0] != "echo" {
		t.Errorf("uses = %v", uses)
	}
}

func TestRun_UnknownToolYieldsErrorResult(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "missing", Args: "{}"}),
		textResponse("ok"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestRun_ToolPanicRecovered(t *testing.T) {
	tool := Tool{
		Name: "boom",
		Call: func(context.Context, string) ToolResult { panic("boom") },
	}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "boom", Args: "{}"}),
		textResponse("recovered"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
}

func TestRun_LLMErrorPropagates(t *testing.T) {
	// Three 503s: after retrying testMaxRetries times it still fails, and the error propagates up.
	tr := &fakeTransport{statuses: []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}}
	llm := testClients(tr)
	_, err := Run(context.Background(), llm, LoopConfig{}, "hi", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if tr.calls != 1+testMaxRetries {
		t.Errorf("calls = %d, want %d (1 + testMaxRetries)", tr.calls, 1+testMaxRetries)
	}
}

func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "hi", LoopHooks{}, nil); err == nil {
		t.Fatal("expected error")
	}
}

// For concurrent multi tool_call within a single step, see loop_concurrency_test.go.

// Multi-step ReAct: step 1 tool result is fed back, step 2 produces the final text.
func TestRun_MultiStepReAct(t *testing.T) {
	tool := Tool{Name: "query", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "data-42"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "query", Args: "{}"}),
		toolResponse(ToolCall{ID: "c2", Name: "query", Args: "{}"}),
		textResponse("final answer"),
	}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	if res.Text != "final answer" {
		t.Errorf("Text = %q", res.Text)
	}
}

// P2-2: when the endpoint does not support thinking, the downgrade happens only once on the first step
// across steps; subsequent steps go straight through without thinking and no longer hit 400.
func TestRun_ThinkingDowngradePersistsAcrossSteps(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	thinkErr := `{"error":{"message":"unknown parameter: reasoning_effort"}}`
	tr := &fakeTransport{
		statuses:  []int{http.StatusBadRequest, http.StatusOK, http.StatusOK},
		responses: []string{thinkErr, toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}), textResponse("done")},
	}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}, Tools: []Tool{tool}}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	// Without downgrade persistence, step 2 would resend thinking → one extra 400 + retry (4 calls total);
	// with persistence the total is 3.
	if len(tr.bodies) != 3 {
		t.Fatalf("calls = %d, want 3 (downgrade happens only on the first step; step 2 should send without thinking)", len(tr.bodies))
	}
	for i, b := range tr.bodies {
		has := strings.Contains(b, "reasoning_effort")
		if i == 0 && !has {
			t.Errorf("body[%d] should carry thinking (first probe): %s", i, b)
		}
		if i != 0 && has {
			t.Errorf("body[%d] should not carry thinking (downgrade should have persisted): %s", i, b)
		}
	}
	// Fix 5: a downgrade occurred → result.ThinkingDowngraded=true (the interaction layer clears baseCfg
	// accordingly; see review P2 cross-turn persistence).
	if !res.ThinkingDowngraded {
		t.Errorf("ThinkingDowngraded should be true after a downgrade occurred")
	}
}

// Minimal mode (BeforeLLM=nil): the core does no context management — huge history is sent verbatim, with
// no compaction and Compacted=false. This is the contract proof of "minimal core + compaction as a plugin":
// attaching no NewCompaction yields an agent without compaction.
func TestRun_NilBeforeLLMIsMinimalNoCompaction(t *testing.T) {
	big := strings.Repeat("x", 1000)
	hist := make([]Message, 20)
	for i := range hist {
		hist[i] = Message{Role: RoleUser, Content: big}
	}
	tr := &fakeTransport{responses: []string{textResponse("done")}}
	llm := testClients(tr)
	res, err := Run(context.Background(), llm, LoopConfig{History: hist}, "x", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Compacted {
		t.Error("minimal mode (nil BeforeLLM) must not compact")
	}
	if tr.calls != 1 {
		t.Errorf("minimal mode: want 1 LLM call, got %d", tr.calls)
	}
	// The whole history enters the request body verbatim (no compaction, no trimming) — had it been
	// compacted it would not contain the full big blob.
	if !strings.Contains(tr.lastBody, big) {
		t.Errorf("minimal mode should send history verbatim, body lacks big blob: %s", tr.lastBody)
	}
}

// OnBudget returns ErrBudgetExceeded → Run terminates immediately and propagates that error (the
// circuit-break contract of the budget plugin).
func TestRun_OnBudgetExceedsStops(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: "{}"}),
		textResponse("done"),
	}}
	llm := testClients(tr)
	// The response usage={1,1} (see toolResponse): the first step accumulates total={1,1}, threshold 1 → circuit-break.
	hooks := LoopHooks{OnBudget: func(_ context.Context, _ int, _ BudgetInput, total *Usage) error {
		if total.InputTokens+total.OutputTokens > 1 {
			return ErrBudgetExceeded
		}
		return nil
	}}
	_, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "x", hooks, nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

// appendMsg timestamping: Ts==0 is auto-stamped (>0); an explicit Ts is preserved.
func TestAppendMsg_Timestamp(t *testing.T) {
	var msgs, newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: "auto"})
	if msgs[0].Ts == 0 {
		t.Error("Ts==0 should be auto-stamped by appendMsg to >0")
	}
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: "manual", Ts: 42})
	if msgs[1].Ts != 42 {
		t.Errorf("explicit Ts should be preserved: got %d, want 42", msgs[1].Ts)
	}
}
