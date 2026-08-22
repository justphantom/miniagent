package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// lengthResponse builds a chat completion that truncated on max_tokens with empty content + reasoning — the R1 trap:
// the model spent the output budget on reasoning_content and returned no usable text.
func lengthResponse(reasoning string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":%q},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, reasoning)
}

// R1 main-loop defense: a length-truncated EMPTY response with reasoning, with thinking on → retry once with thinking
// dropped; the second response supplies real text. Without the fix the empty content is returned as the final answer.
func TestCallLLM_LengthEmptyRetriesWithThinkingDropped(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusOK, body: lengthResponse("thinking hard, burning the output budget...")},
		{status: http.StatusOK, body: textResponse("the real answer")},
	}}
	llm := testClients(tr)
	resp, downgraded, requests, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("callLLMWithDowngrade: %v", err)
	}
	if resp.Text != "the real answer" {
		t.Errorf("Text = %q, want the retry's real answer", resp.Text)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop (from retry)", resp.FinishReason)
	}
	if !downgraded {
		t.Error("downgraded = false, want true (length rescue drops thinking)")
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2 (initial + rescue)", requests)
	}
	if tr.calls != 2 {
		t.Errorf("transport calls = %d, want 2", tr.calls)
	}
	if strings.Contains(tr.bodies[1], "reasoning_effort") {
		t.Errorf("rescue body should drop thinking: %s", tr.bodies[1])
	}
}

// R1 no-rescue: thinking off + length+empty+reasoning → no retry (nothing to drop), empty text returned as-is. This
// pins that the rescue does not fire pointlessly and documents that R1 stays partially open for the no-thinking case
// (intrinsic-reasoning models where reasoning isn't togglable — the rescue can't help).
func TestCallLLM_LengthEmptyNoThinking_NoRescue(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusOK, body: lengthResponse("thinking...")},
	}}
	llm := testClients(tr)
	resp, downgraded, requests, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m"}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("callLLMWithDowngrade: %v", err)
	}
	if resp.Text != "" {
		t.Errorf("Text = %q, want empty (no rescue, returned as-is)", resp.Text)
	}
	if downgraded || requests != 1 || tr.calls != 1 {
		t.Errorf("no rescue expected: downgraded=%v requests=%d calls=%d", downgraded, requests, tr.calls)
	}
}

// R1 with wire-state-only evidence: some endpoints return no reasoning_content text, only the opaque
// ReasoningState — that alone still proves the budget went to thinking, so the rescue must fire (L16).
func TestCallLLM_LengthEmptyReasoningStateOnly_Rescued(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusOK, body: `{"choices":[{"message":{"role":"assistant","content":"","reasoning_state":"opaque-wire-state"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`},
		{status: http.StatusOK, body: textResponse("the real answer")},
	}}
	llm := testClients(tr)
	resp, downgraded, requests, err := callLLMWithDowngrade(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium", Thinking: &ThinkingMapping{Field: "reasoning_effort", Map: map[string]string{"medium": "medium"}}}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("callLLMWithDowngrade: %v", err)
	}
	if resp.Text != "the real answer" {
		t.Errorf("Text = %q, want the retry's real answer", resp.Text)
	}
	if !downgraded || requests != 2 || tr.calls != 2 {
		t.Errorf("rescue expected: downgraded=%v requests=%d calls=%d", downgraded, requests, tr.calls)
	}
}
