package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/justphantom/miniagent/internal/miniagent"
)

type responseError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

func (e responseError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

type responseDocument struct {
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Error             *responseError    `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type outputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text,omitempty"`
		Refusal string `json:"refusal,omitempty"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text,omitempty"`
	} `json:"summary"`
}

func parseResponse(raw []byte) (miniagent.Response, error) {
	var doc responseDocument
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return miniagent.Response{}, fmt.Errorf("parse response: %w", err)
	}
	if doc.Error != nil {
		return miniagent.Response{}, fmt.Errorf("response failed: %w", *doc.Error)
	}
	switch doc.Status {
	case "completed", "incomplete":
	case "failed":
		return miniagent.Response{}, errors.New("response failed without error details")
	default:
		return miniagent.Response{}, fmt.Errorf("unsupported response status %q", doc.Status)
	}
	if len(doc.Output) == 0 && doc.Status == "completed" {
		return miniagent.Response{}, errors.New("response has no output items")
	}
	out := miniagent.Response{}
	var reasoningItems []json.RawMessage
	for _, rawItem := range doc.Output {
		var item outputItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return miniagent.Response{}, fmt.Errorf("parse output item: %w", err)
		}
		switch item.Type {
		case "reasoning":
			reasoningItems = append(reasoningItems, rawItem)
			for _, summary := range item.Summary {
				if out.Reasoning != "" && summary.Text != "" {
					out.Reasoning += "\n"
				}
				out.Reasoning += summary.Text
			}
		case "message":
			for _, content := range item.Content {
				out.Text += content.Text
				if content.Refusal != "" {
					out.Text += content.Refusal
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{ID: item.CallID, Name: item.Name, Args: item.Arguments})
		default:
			return miniagent.Response{}, fmt.Errorf("unsupported response output item %q", item.Type)
		}
	}
	if len(reasoningItems) > 0 {
		state, err := json.Marshal(reasoningItems)
		if err != nil {
			return miniagent.Response{}, fmt.Errorf("encode reasoning state: %w", err)
		}
		out.ReasoningState = string(state)
	}
	out.FinishReason = finishReason(doc, out.ToolCalls)
	if doc.Usage != nil {
		out.Usage = miniagent.Usage{InputTokens: doc.Usage.InputTokens, OutputTokens: doc.Usage.OutputTokens}
	}
	return out, nil
}

func finishReason(doc responseDocument, calls []miniagent.ToolCall) string {
	if doc.Status == "completed" {
		if len(calls) > 0 {
			return "tool_calls"
		}
		return "stop"
	}
	if doc.IncompleteDetails != nil && doc.IncompleteDetails.Reason != "" {
		if doc.IncompleteDetails.Reason == "max_output_tokens" {
			return "length"
		}
		return doc.IncompleteDetails.Reason
	}
	return "length"
}
