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

	Message string `json:"message,omitempty"`
}

// StreamEmitFunc returns an EmitFunc that writes Signal events as NDJSON.
//
// 错误契约：返回的 error 会沿 emitSignal → Run 一路上抛到调用方。这意味着
// 当下游 io.Writer 不可写（如 stdout 管道被消费者提前关闭）时，Run 会立刻
// 终止返回该 error，而非继续后续 LLM 调用浪费 token。调用方应区分：
//   - error 包含 "broken pipe" / "file already closed" → 消费者断开，非 LLM 故障
//   - 其他 error → 来自 LLM 调用或 ctx 取消
func StreamEmitFunc(w io.Writer, verbose bool) EmitFunc {
	enc := json.NewEncoder(w)
	return func(sig Signal) error {
		// SignalText 是核心流式输出，始终透传；verbose 只控制 tool_result。
		if !verbose && sig.Kind != SignalToolUse && sig.Kind != SignalText {
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
	case SignalText:
		return streamEvent{Type: "text", Text: sig.Text}
	default:
		return streamEvent{Type: string(sig.Kind), Name: sig.Name}
	}
}
