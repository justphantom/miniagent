package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestBuildBody_ProjectsResponsesInput(t *testing.T) {
	state := `[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}]`
	req := miniagent.Request{
		Model:  "gpt-test",
		System: "system prompt",
		Messages: []miniagent.Message{
			{Role: miniagent.RoleUser, Content: "question"},
			{Role: miniagent.RoleAssistant, Content: "checking", ReasoningState: state, ToolCalls: []miniagent.ToolCall{{ID: "call_1", Name: "read", Args: `{"path":"x"}`}}, Kind: miniagent.KindSummary, Usage: &miniagent.Usage{}},
			{Role: miniagent.RoleTool, ToolCallID: "call_1", Content: "result"},
		},
		MaxTokens:     123,
		Tools:         []miniagent.Tool{{Name: "read", Description: "read file", Parameters: map[string]any{"type": "object"}}},
		ThinkingLevel: "deep",
		Thinking:      &miniagent.ThinkingMapping{Field: "reasoning", Map: map[string]string{"deep": "high"}},
	}
	body, err := buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["instructions"] != "system prompt" || got["max_output_tokens"] != float64(123) {
		t.Errorf("instructions/max_output_tokens = %v/%v", got["instructions"], got["max_output_tokens"])
	}
	if got["store"] != false {
		t.Errorf("store = %v, want false", got["store"])
	}
	include, _ := got["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v", got["include"])
	}
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning = %v, want effort high", got["reasoning"])
	}
	input, _ := got["input"].([]any)
	if len(input) != 5 {
		t.Fatalf("input len = %d, body=%s", len(input), body)
	}
	if input[0].(map[string]any)["content"] != "question" {
		t.Errorf("first input = %v", input[0])
	}
	if input[1].(map[string]any)["type"] != "reasoning" || input[1].(map[string]any)["encrypted_content"] != "opaque" {
		t.Errorf("reasoning input = %v", input[1])
	}
	call, _ := input[3].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "read" {
		t.Errorf("function call input = %v", call)
	}
	tools, _ := got["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "read" {
		t.Errorf("tool = %v", tool)
	}
	for _, leaked := range []string{`"kind"`, `"is_error"`, `"usage"`, `"ts"`} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("body leaks session field %s: %s", leaked, body)
		}
	}
}

func TestBuildBody_RejectsInvalidReasoningState(t *testing.T) {
	_, err := buildBody(miniagent.Request{Messages: []miniagent.Message{{Role: miniagent.RoleAssistant, ReasoningState: `[{"type":`}}})
	if err == nil || !strings.Contains(err.Error(), "reasoning_state") {
		t.Fatalf("err = %v, want reasoning_state error", err)
	}
}
