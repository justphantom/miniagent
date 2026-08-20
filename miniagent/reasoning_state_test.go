package miniagent

import (
	"context"
	"encoding/json"
	"testing"
)

type reasoningStateLLM struct {
	requests []Request
	calls    int
}

func (l *reasoningStateLLM) Do(_ context.Context, req Request) (Response, error) {
	l.requests = append(l.requests, req)
	l.calls++
	if l.calls == 1 {
		return Response{ReasoningState: `[{"type":"reasoning","encrypted_content":"opaque"}]`, ToolCalls: []ToolCall{{ID: "call_1", Name: "echo", Args: "{}"}}}, nil
	}
	return Response{Text: "done"}, nil
}

func (l *reasoningStateLLM) DoStream(ctx context.Context, req Request, _ func(Delta) error) (Response, error) {
	return l.Do(ctx, req)
}

func TestRun_PersistsReasoningStateAcrossToolCall(t *testing.T) {
	llm := &reasoningStateLLM{}
	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{{Name: "echo", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "ok"} }}}}, "q", LoopHooks{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("requests = %d", len(llm.requests))
	}
	second := llm.requests[1].Messages
	found := false
	for _, msg := range second {
		if msg.Role == RoleAssistant && len(msg.ToolCalls) == 1 && msg.ReasoningState != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning state not replayed: %+v", second)
	}
	var persisted bool
	for _, msg := range res.Messages {
		if msg.Role == RoleAssistant && len(msg.ToolCalls) == 1 && msg.ReasoningState != "" {
			persisted = true
		}
	}
	if !persisted {
		t.Fatalf("reasoning state not persisted: %+v", res.Messages)
	}
}

func TestMessageReasoningStateJSONBackwardCompatible(t *testing.T) {
	var msg struct {
		ReasoningState string `json:"reasoning_state,omitempty"`
	}
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"ok"}`), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.ReasoningState != "" {
		t.Errorf("ReasoningState = %q, want empty", msg.ReasoningState)
	}
	encoded, err := json.Marshal(struct {
		Role           string `json:"role"`
		Content        string `json:"content"`
		ReasoningState string `json:"reasoning_state,omitempty"`
	}{Role: RoleAssistant, Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"role":"assistant","content":"ok"}` {
		t.Errorf("encoded = %s", encoded)
	}
}
