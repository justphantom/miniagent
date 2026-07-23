package miniagent

import (
	"encoding/json"
	"io"
)

// toolUseEvent 是每次工具调用的 NDJSON 事件。
type toolUseEvent struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input string `json:"input"`
}

// resultEvent 是终态事件。text/model/input_tokens/output_tokens/steps 均
// 不带 omitempty，为 0 也会出现键名，方便消费方稳定 parse。
type resultEvent struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Steps        int    `json:"steps"`
}

// errorEvent 是终态错误事件。
type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// StreamEmitFunc returns an EmitFunc that writes tool_use Signal events as NDJSON.
//
// 错误契约：返回的 error 会沿 emitSignal → Run 一路上抛到调用方。当下游
// io.Writer 不可写（如 stdout 管道被消费者提前关闭）时，Run 会立刻终止
// 返回该 error，而非继续后续 LLM 调用浪费 token。
func StreamEmitFunc(w io.Writer) EmitFunc {
	enc := json.NewEncoder(w)
	return func(sig Signal) error {
		if sig.Kind != SignalToolUse {
			return nil
		}
		return enc.Encode(toolUseEvent{Type: "tool_use", Name: sig.Name, Input: sig.Input})
	}
}

// EmitResult writes the terminal result event to w.
func EmitResult(w io.Writer, result Result, model string) error {
	return json.NewEncoder(w).Encode(resultEvent{
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
	return json.NewEncoder(w).Encode(errorEvent{Type: "error", Message: msg})
}
