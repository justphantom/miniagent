package responses

import (
	"encoding/json"
	"fmt"

	"github.com/justphantom/miniagent/internal/miniagent"
)

const maxRequestBodyBytes = 4 << 20

type inputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type functionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func estimateRequestBodySize(req miniagent.Request) int64 {
	size := int64(256 + len(req.System))
	for _, m := range req.Messages {
		size += int64(len(m.Role) + len(m.Content) + len(m.Reasoning) + len(m.ReasoningState) + len(m.ToolCallID))
		for _, tc := range m.ToolCalls {
			size += int64(len(tc.ID) + len(tc.Name) + len(tc.Args))
		}
	}
	for _, t := range req.Tools {
		size += int64(len(t.Name)+len(t.Description)) + 64
	}
	return size * 13 / 10
}

func buildBody(req miniagent.Request) ([]byte, error) {
	estimated := estimateRequestBodySize(req)
	if estimated > maxRequestBodyBytes {
		return nil, fmt.Errorf("estimated request %d bytes exceeds upper limit %d", estimated, maxRequestBodyBytes)
	}
	input, err := projectInput(req.Messages)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":   req.Model,
		"input":   input,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}
	if req.System != "" {
		payload["instructions"] = req.System
	}
	if req.MaxTokens > 0 {
		payload["max_output_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]functionTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, functionTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		}
		payload["tools"] = tools
	}
	addThinking(payload, req)
	if req.Stream {
		payload["stream"] = true
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return nil, fmt.Errorf("request body %d bytes exceeds upper limit %d", len(body), maxRequestBodyBytes)
	}
	return body, nil
}

func projectInput(messages []miniagent.Message) ([]any, error) {
	input := make([]any, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case miniagent.RoleSystem, miniagent.RoleUser, miniagent.RoleAssistant:
			state, err := reasoningItems(m.ReasoningState)
			if err != nil {
				return nil, err
			}
			input = append(input, state...)
			if m.Content != "" || len(m.ToolCalls) == 0 {
				input = append(input, inputMessage{Role: m.Role, Content: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, functionCallItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: tc.Args})
			}
		case miniagent.RoleTool:
			input = append(input, functionOutputItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	return input, nil
}

func reasoningItems(state string) ([]any, error) {
	if state == "" {
		return nil, nil
	}
	raw := []byte(state)
	if raw[0] != '[' {
		raw = []byte("[" + state + "]")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("invalid reasoning_state: %w", err)
	}
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out, nil
}

func addThinking(payload map[string]any, req miniagent.Request) {
	if req.ThinkingLevel == "" || req.ThinkingLevel == miniagent.ThinkingOff || req.Thinking == nil || req.Thinking.Field != "reasoning" {
		return
	}
	effort := req.ThinkingLevel
	if mapped, ok := req.Thinking.Map[req.ThinkingLevel]; ok {
		effort = mapped
	}
	payload["reasoning"] = map[string]any{"effort": effort}
}
