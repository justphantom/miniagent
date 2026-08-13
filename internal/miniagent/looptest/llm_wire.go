package looptest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// ---- Test-side copies of the openai package's wire / retry logic (kept in sync with the openai package implementation)----

const (
	MaxRetries       = 2
	RetryBaseDelay   = 500 * time.Millisecond
	RetryMaxDelay    = 8 * time.Second
	MaxChatBodyBytes = 4 << 20
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

// BuildChatBody replicates openai.testBuildChatBody: builds an OpenAI-compatible wire body so that the
// LastBody/Bodies recorded by FakeTransport match the real ChatClient (including "role":"system" / reasoning_effort).
func BuildChatBody(req miniagent.Request) ([]byte, error) {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: miniagent.RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, Content: m.Content, ReasoningContent: m.Reasoning, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			ctc := chatToolCall{ID: tc.ID, Type: "function"}
			ctc.Fn.Name = tc.Name
			ctc.Fn.Args = tc.Args
			cm.ToolCalls = append(cm.ToolCalls, ctc)
		}
		msgs = append(msgs, cm)
	}
	payload := map[string]any{"model": req.Model, "messages": msgs}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.ThinkingLevel != "" && req.ThinkingLevel != miniagent.ThinkingOff && req.Thinking != nil {
		field, val := req.Thinking.Field, req.ThinkingLevel
		if mapped, ok := req.Thinking.Map[req.ThinkingLevel]; ok {
			val = mapped
		}
		payload[field] = val
	}
	if req.Stream {
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

// ParseChatResponse replicates openai.testParseChatResponse.
func ParseChatResponse(raw []byte) (miniagent.Response, error) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					ID       string `json:"id"`
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
	if err := json.Unmarshal(raw, &v); err != nil {
		return miniagent.Response{}, fmt.Errorf("parse response: %w", err)
	}
	if len(v.Choices) == 0 {
		return miniagent.Response{}, errors.New("llm response has no choices")
	}
	ch := v.Choices[0]
	out := miniagent.Response{Text: ch.Message.Content, Reasoning: ch.Message.ReasoningContent, FinishReason: ch.FinishReason}
	if out.Reasoning == "" {
		out.Reasoning = ch.Message.Reasoning
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	if v.Usage != nil {
		out.Usage = miniagent.Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}

// ShouldRetryStatus replicates openai.shouldRetryStatus.
func ShouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// IsThinkingError replicates openai.isThinkingError (tightened version: strong-signal field names + the weak-signal thinking&unknown combination).
func IsThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning_effort_level") {
		return true
	}
	hasThinking := strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking")
	hasUnknown := strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unexpected argument")
	return hasThinking && hasUnknown
}

// ParseRetryAfter replicates openai.parseRetryAfter.
func ParseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return -1
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return -1
}

// CapRetryDelay replicates openai.capRetryDelay.
func CapRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > RetryMaxDelay {
		backoff = RetryMaxDelay
	}
	return backoff
}

// SleepCtx replicates openai.sleepCtx (respects ctx cancellation).
func SleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
