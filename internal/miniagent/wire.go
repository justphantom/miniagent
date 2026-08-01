// wire.go 是 OpenAI Chat Completions schema 的序列化层。
// chatMessage / chatToolCall 与 types.go 的 Message / ToolCall 字段刻意重复：
// 上层 domain 类型不绑死特定厂商的 JSON 形状（嵌套 function 对象、snake_case
// 字段名），新增字段时需同步两处并保持与 OpenAI API 字段顺序、命名一致。
package miniagent

import (
	"encoding/json"
)

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Fn   struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

func buildChatBody(req Request) ([]byte, error) {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: roleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		// 回灌 reasoning：assistant 的思考链以 reasoning_content 发回（DeepSeek 兼容）。
		cm := chatMessage{Role: m.Role, Content: m.Content, ReasoningContent: m.Reasoning, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			ctc := chatToolCall{ID: tc.ID, Type: "function"}
			ctc.Fn.Name = tc.Name
			ctc.Fn.Args = tc.Args
			cm.ToolCalls = append(cm.ToolCalls, ctc)
		}
		msgs = append(msgs, cm)
	}
	payload := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Stream {
		// stream_options.include_usage：让末 chunk 携带 usage（计费/熔断仍以它为准）。
		payload["stream"] = true
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		funcs := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			funcs = append(funcs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		payload["tools"] = funcs
	}
	return json.Marshal(payload)
}
