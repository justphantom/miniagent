package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// maxRequestBodyBytes caps the serialized request body (mirrors the openai provider and the response cap).
const maxRequestBodyBytes = 4 << 20 // 4 MiB

// estimateRequestBodySize roughly estimates the JSON bytes buildBody will produce, to reject oversized
// requests before marshal (mirrors openai wire.go). Conservative: 1.3× the total string length + envelope.
func estimateRequestBodySize(req miniagent.Request) int64 {
	size := int64(256) // fixed-field overhead for model, max_tokens, stream, tools, etc.
	size += int64(len(req.System))
	for _, m := range req.Messages {
		size += int64(len(m.Role) + len(m.Content) + len(m.Reasoning) + len(m.ToolCallID))
		for _, tc := range m.ToolCalls {
			size += int64(len(tc.ID) + len(tc.Name) + len(tc.Args))
		}
	}
	for _, t := range req.Tools {
		size += int64(len(t.Name)+len(t.Description)) + 64 // parameters only minimally estimated
	}
	return size * 13 / 10
}

// buildBody serializes a Request to the Anthropic Messages API JSON shape. It performs the wire-boundary
// lossy projection from the flat core types (Message.Content/Reasoning single strings, flat ToolCalls) to
// Anthropic's typed content-block arrays. cache toggles prompt-caching cache_control breakpoints (system +
// last user message). thinking is rendered from the provider ThinkingMapping (see resolveThinking).
func buildBody(req miniagent.Request, cache bool) ([]byte, error) {
	size := estimateRequestBodySize(req)
	if size > maxRequestBodyBytes {
		return nil, fmt.Errorf("estimated request %d bytes exceeds upper limit %d", size, maxRequestBodyBytes)
	}
	payload := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens, // Anthropic mandates max_tokens (config-side validated > 0 for kind=anthropic)
	}
	if req.System != "" {
		payload["system"] = systemBlocks(req.System, cache)
	}
	payload["messages"] = projectMessages(req.Messages, cache)
	if len(req.Tools) > 0 {
		payload["tools"] = toolBlocks(req.Tools)
	}
	resolveThinking(payload, req)
	if req.Stream {
		payload["stream"] = true // no stream_options: Anthropic always emits usage in message_start/message_delta
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

// messagesResponse extracts the fields the loop needs from a non-streaming Messages API response:
// content blocks (text/thinking/tool_use), stop_reason, usage.
type messagesResponse struct {
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text,omitempty"`
		Thinking string          `json:"thinking,omitempty"`
		ID       string          `json:"id,omitempty"`
		Name     string          `json:"name,omitempty"`
		Input    json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	} `json:"usage"`
}

// parseResponse maps a non-streaming Messages API body to the core Response. text blocks concatenate into
// Text, thinking blocks into Reasoning, tool_use blocks into ToolCalls. Cache tokens fold into InputTokens
// (the core Usage has no cache field; the total input context is what compaction's overflow check needs).
func parseResponse(raw []byte) (miniagent.Response, error) {
	var v messagesResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		return miniagent.Response{}, fmt.Errorf("parse response: %w", err)
	}
	if len(v.Content) == 0 {
		return miniagent.Response{}, errors.New("anthropic response has no content blocks")
	}
	out := miniagent.Response{}
	for _, blk := range v.Content {
		switch blk.Type {
		case "text":
			out.Text += blk.Text
		case "thinking":
			out.Reasoning += blk.Thinking
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{
				ID:   blk.ID,
				Name: blk.Name,
				Args: argsToString(blk.Input),
			})
		}
	}
	out.FinishReason = mapStopReason(v.StopReason)
	out.Usage = miniagent.Usage{
		InputTokens:  v.Usage.InputTokens + v.Usage.CacheCreationInputTokens + v.Usage.CacheReadInputTokens,
		OutputTokens: v.Usage.OutputTokens,
	}
	return out, nil
}
