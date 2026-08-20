package session

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent/looptest"
	"github.com/justphantom/miniagent/miniagent"
)

// maxIterations aligns with the core default iteration cap (miniagent.maxIterations is unexported), for the hit-the-limit assertions in this file.
const maxIterations = 20

func TestResolveSessionPath(t *testing.T) {
	// valid id → {dir}/{id}.jsonl
	p, err := ResolveSessionPath("mysess", ".miniagent/sessions")
	if err != nil || p != filepath.Join(".miniagent/sessions", "mysess.jsonl") {
		t.Errorf("id resolution: p=%q err=%v", p, err)
	}
	// a generated-style id (letters/digits/-) is valid.
	if _, err := ResolveSessionPath("20260805-143022-abc123", "dir"); err != nil {
		t.Errorf("generated-style id should be valid: %v", err)
	}
	// illegal characters (path separators, dots, spaces, etc.) must error.
	for _, bad := range []string{"s.json", "./x/s", "a/b", "a\\b", "a b", ".."} {
		if _, err := ResolveSessionPath(bad, "dir"); err == nil {
			t.Errorf("bad id %q should error", bad)
		}
	}
	// empty errors.
	if _, err := ResolveSessionPath("", "dir"); err == nil {
		t.Error("empty id should error")
	}
	// empty dir errors.
	if _, err := ResolveSessionPath("mysess", ""); err == nil {
		t.Error("id without dir should error")
	}
}

// NewMessages contains only this turn's additions (excluding History); Messages includes the History prefix.
func TestRun_NewMessagesExcludesHistory(t *testing.T) {
	tool := miniagent.Tool{Name: "echo", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "echoed"} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		looptest.TextResponse("done"),
	}}
	llm := looptest.NewFakeLLM(tr)
	history := []miniagent.Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "oldans"},
	}
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: history}, "newq", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(res.NewMessages) != len(wantRoles) {
		t.Fatalf("NewMessages len = %d, want %d (%+v)", len(res.NewMessages), len(wantRoles), res.NewMessages)
	}
	for i, w := range wantRoles {
		if res.NewMessages[i].Role != w {
			t.Errorf("NewMessages[%d].Role = %q, want %q", i, res.NewMessages[i].Role, w)
		}
	}
	if len(res.Messages) != len(history)+len(wantRoles) {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), len(history)+len(wantRoles))
	}
}

// History is prepended before the new prompt and sent to the LLM; miniagent.Run does not mutate the caller's History.
func TestRun_HistoryPrefixSent(t *testing.T) {
	tr := &looptest.FakeTransport{Responses: []string{looptest.TextResponse("a2")}}
	llm := looptest.NewFakeLLM(tr)
	history := []miniagent.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{History: history}, "q2", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	i1 := strings.Index(tr.LastBody, "q1")
	i2 := strings.Index(tr.LastBody, "a1")
	i3 := strings.Index(tr.LastBody, "q2")
	if i1 < 0 || i2 < 0 || i3 < 0 || i1 >= i2 || i2 >= i3 {
		t.Errorf("history not sent in order q1<a1<q2: %s", tr.LastBody)
	}
	if len(history) != 2 {
		t.Errorf("caller history mutated: len = %d", len(history))
	}
	want := []miniagent.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	if len(res.Messages) != len(want) {
		t.Fatalf("Messages len = %d, want %d: %+v", len(res.Messages), len(want), res.Messages)
	}
	// After §P0-B, newly produced assistant messages carry miniagent.Usage/Ts and new user messages carry Ts (non-deterministic),
	// so only Role+Content are validated here; a full reflect.DeepEqual is no longer used (history q1/a1 still lack
	// miniagent.Usage/Ts, and a deep compare would be destabilized by the new fields).
	for i, w := range want {
		if res.Messages[i].Role != w.Role || res.Messages[i].Content != w.Content {
			t.Errorf("Messages[%d] = {Role:%q Content:%q}, want {Role:%q Content:%q}",
				i, res.Messages[i].Role, res.Messages[i].Content, w.Role, w.Content)
		}
	}
}

// The final assistant text must enter Messages (continuing the conversation depends on the previous turn's answer).
func TestRun_FinalTextAppendedToMessages(t *testing.T) {
	tr := &looptest.FakeTransport{Responses: []string{looptest.TextResponse("final answer")}}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{}, "q", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "assistant" || last.Content != "final answer" {
		t.Errorf("last message = %+v", last)
	}
}

// Two-turn continuation: turn 1's complete transcript is passed as History into turn 2; the request body contains all 4 message types in order.
func TestRun_ContinuationSendsFullTranscript(t *testing.T) {
	tool := miniagent.Tool{Name: "echo", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "echoed"} }}
	tr := &looptest.FakeTransport{Responses: []string{
		looptest.ToolResponse(miniagent.ToolCall{ID: "c1", Name: "echo", Args: `{"x":1}`}),
		looptest.TextResponse("turn 1 reply"),
	}}
	llm := looptest.NewFakeLLM(tr)
	r1, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}}, "turn 1", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run turn1: %v", err)
	}

	tr2 := &looptest.FakeTransport{Responses: []string{looptest.TextResponse("turn 2 reply")}}
	llm = looptest.NewFakeLLM(tr2)
	_, err = miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}, History: r1.Messages}, "turn 2", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run turn2: %v", err)
	}
	var body struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(tr2.LastBody), &body); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	var roles []string
	for _, m := range body.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "assistant", "user"}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("turn2 request roles = %v, want %v", roles, want)
	}
	if !strings.Contains(tr2.LastBody, "turn 1 reply") {
		t.Errorf("turn2 request missing turn1 final text: %s", tr2.LastBody)
	}
}

// LLM errors: Result.Messages still brings back the accumulated history (including this turn's user prompt).
func TestRun_ErrorStillReturnsMessages(t *testing.T) {
	tr := &looptest.FakeTransport{Statuses: []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{}, "hi", miniagent.LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v", res.Messages)
	}
}

// Hitting maxIterations: Messages contains all accumulated tool round-trips + the summary request injected at the end.
// Option B: after the iterLimit step's tool call, summaryRequestPrompt is injected, so there is one extra system message.
func TestRun_MaxIterationsReturnsMessages(t *testing.T) {
	tool := miniagent.Tool{Name: "loop", Call: func(context.Context, string) miniagent.ToolResult { return miniagent.ToolResult{Output: "x"} }}
	responses := make([]string, maxIterations+2)
	for i := range responses {
		responses[i] = looptest.ToolResponse(miniagent.ToolCall{ID: "c", Name: "loop", Args: "{}"})
	}
	tr := &looptest.FakeTransport{Responses: responses}
	llm := looptest.NewFakeLLM(tr)
	res, err := miniagent.Run(context.Background(), llm, miniagent.LoopConfig{Tools: []miniagent.Tool{tool}}, "x", miniagent.LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("miniagent.Run: %v", err)
	}
	// 1 (user) + 2*maxIterations (assistant+tool, maxIterations turns each); the summary guidance message does not enter the transcript.
	if want := 1 + 2*maxIterations; len(res.Messages) != want {
		t.Errorf("Messages len = %d, want %d", len(res.Messages), want)
	}
	// The last message should be the most recent tool result (the summary is sent via temporary reqMsgs and does not pollute Messages).
	if res.Messages[len(res.Messages)-1].Role != miniagent.RoleTool {
		t.Errorf("last message role = %q, want %q", res.Messages[len(res.Messages)-1].Role, miniagent.RoleTool)
	}
}
