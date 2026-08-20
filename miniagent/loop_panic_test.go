package miniagent

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Run has a top-level recover so a panic in any user hook is converted to an error return instead of crashing the
// process, AND the turn's NewMessages still survive the defer (so session persistence can save them). Before the fix
// only safeCall (tools) and callLLMOnce (LLM) recovered; the 7 hook call sites were unprotected, so one nil-deref /
// closed-pipe panic in a long autonomous chain lost the whole un-persisted turn.
func TestRun_HookPanicRecovered(t *testing.T) {
	llm := testClients(&recordingTransport{plan: []transportResp{{status: http.StatusOK, body: textResponse("done")}}})
	panicBefore := func(ctx context.Context, in StepInput) (StepOutput, error) {
		panic("boom from hook")
	}
	res, err := Run(context.Background(), llm, LoopConfig{Model: "m"}, "hello", LoopHooks{BeforeLLM: panicBefore}, nil)
	if err == nil {
		t.Fatal("expected error from hook panic, got nil")
	}
	if !strings.Contains(err.Error(), "run panicked") {
		t.Errorf("err should wrap 'run panicked', got: %v", err)
	}
	// The defer must still populate result so the turn's transcript survives for session persistence: the user prompt
	// is appended before BeforeLLM fires, so NewMessages is non-empty even though the step panicked mid-hook.
	if len(res.NewMessages) == 0 {
		t.Error("NewMessages should survive the panic (defer still ran), got empty")
	}
}
