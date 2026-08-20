// Package openai is the OpenAI-compatible Chat Completions provider implementation: request serialization (wire),
// non-streaming client (ChatClient), streaming client (StreamClient), SSE parsing (stream_parse),
// models list and retry/backoff (retry). It is the default implementation of the core LLM/Doer interface — the core
// loop is decoupled from specific vendors through it; a custom provider only needs to implement miniagent.LLM to replace it,
// with zero changes to the core.
//
// wire.go is the serialization layer for the OpenAI Chat Completions schema.
// chatMessage / chatToolCall deliberately duplicate fields from miniagent.Message / miniagent.ToolCall:
// upper-layer domain types are not locked to a specific vendor's JSON shape (nested function objects, snake_case
// field names); when adding new fields you must keep both in sync and consistent with the OpenAI API field order and naming.
// chatMessage has no Kind: session-layer markers do not leak to the LLM (built independently by buildChatBody).
package openai

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/justphantom/miniagent/miniagent"
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

// maxRequestBodyBytes is the byte upper limit for the chat completion request body, aligned with the response upper limit.
// Requests exceeding this value are rejected outright to avoid oversized-request OOM/burning money.
const maxRequestBodyBytes = 4 << 20

// estimateRequestBodySize roughly estimates the number of JSON bytes that buildChatBody will produce, used to intercept
// oversized requests before marshal to avoid OOM. Estimated as 1.3x the total string length + a fixed envelope overhead,
// conservatively biased.
func estimateRequestBodySize(req miniagent.Request) int64 {
	size := int64(256) // fixed-field overhead for model, max_tokens, stream, stream_options, tools, etc.
	size += int64(len(req.System))
	for _, m := range req.Messages {
		size += int64(len(m.Role) + len(m.Content) + len(m.Reasoning) + len(m.ToolCallID))
		for _, tc := range m.ToolCalls {
			size += int64(len(tc.ID) + len(tc.Name) + len(tc.Args))
		}
	}
	for _, t := range req.Tools {
		size += int64(len(t.Name) + len(t.Description))
		// parameters is already an arbitrary type; only a minimal estimate here — messages are what really get large.
		size += 64
	}
	return size * 13 / 10
}

func buildChatBody(req miniagent.Request) ([]byte, error) {
	size := estimateRequestBodySize(req)
	if size > maxRequestBodyBytes {
		return nil, fmt.Errorf("estimated request %d bytes exceeds upper limit %d", size, maxRequestBodyBytes)
	}
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: miniagent.RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		// Re-feed reasoning: the assistant's chain of thought is sent back as reasoning_content (DeepSeek-compatible).
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
	// Thinking level (pinned): must go through provider.Thinking (field+map); req.Thinking==nil → not sent (defensive:
	// the config validation path guarantees that when defaults.thinking≠off the provider must declare it). Empty/ThinkingOff not written.
	if req.ThinkingLevel != "" && req.ThinkingLevel != miniagent.ThinkingOff && req.Thinking != nil {
		field, val := req.Thinking.Field, req.ThinkingLevel
		if mapped, ok := req.Thinking.Map[req.ThinkingLevel]; ok {
			val = mapped
		}
		payload[field] = val
	}
	if req.Stream {
		// stream_options.include_usage: lets the final chunk carry usage (billing/circuit-breaking still rely on it).
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
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return nil, fmt.Errorf("request body %d bytes exceeds upper limit %d", len(body), maxRequestBodyBytes)
	}
	return body, nil
}

// chatCompletionResponse only extracts the fields the loop needs: the message of the first choice
// (content + tool_calls), finish_reason, usage.
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func parseChatResponse(raw []byte) (miniagent.Response, error) {
	var v chatCompletionResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		return miniagent.Response{}, fmt.Errorf("parse response: %w", err)
	}
	out := miniagent.Response{}
	if len(v.Choices) == 0 {
		// Empty choices means an endpoint anomaly (content filtering/proxy failure); a silent zero value would make the upper
		// layer treat it as a "successful empty answer" (exit code 0, empty text), so it must be reported as an error.
		return miniagent.Response{}, errors.New("llm response has no choices")
	}
	ch := v.Choices[0]
	out.Text = ch.Message.Content
	// Dual compatibility: DeepSeek family uses reasoning_content, OpenAI o-series uses reasoning; the former takes precedence.
	out.Reasoning = ch.Message.ReasoningContent
	if out.Reasoning == "" {
		out.Reasoning = ch.Message.Reasoning
	}
	out.FinishReason = ch.FinishReason
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	if v.Usage != nil {
		out.Usage = miniagent.Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}
