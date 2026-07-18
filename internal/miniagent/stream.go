package miniagent

import (
	"encoding/json"
	"io"
)

// streamEvent is one line of NDJSON output.
type streamEvent struct {
	Type string `json:"type"`

	Name    string `json:"name,omitempty"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	Text         string `json:"text,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	Steps        int    `json:"steps,omitempty"`

	Incomplete bool   `json:"incomplete,omitempty"`
	Message    string `json:"message,omitempty"`
}

// StreamEmitFunc returns an EmitFunc that writes Signal events as NDJSON.
func StreamEmitFunc(w io.Writer, verbose bool) EmitFunc {
	enc := json.NewEncoder(w)
	return func(sig Signal) error {
		if !verbose && sig.Kind != SignalToolUse {
			return nil
		}
		return enc.Encode(signalToStreamEvent(sig))
	}
}

// EmitResult writes the terminal result event to w.
func EmitResult(w io.Writer, result Result, model string) error {
	return json.NewEncoder(w).Encode(streamEvent{
		Type:         "result",
		Text:         result.Text,
		Model:        model,
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		Steps:        result.Steps,
		Incomplete:   result.Incomplete,
	})
}

// EmitError writes the terminal error event to w.
func EmitError(w io.Writer, msg string) error {
	return json.NewEncoder(w).Encode(streamEvent{Type: "error", Message: msg})
}

func signalToStreamEvent(sig Signal) streamEvent {
	switch sig.Kind {
	case SignalToolUse:
		return streamEvent{Type: "tool_use", Name: sig.Name, Input: sig.Input}
	case SignalToolResult:
		return streamEvent{Type: "tool_result", Name: sig.Name, Input: sig.Input, Output: sig.Output, IsError: sig.IsError}
	default:
		return streamEvent{Type: string(sig.Kind), Name: sig.Name}
	}
}
