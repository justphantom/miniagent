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

// resultEvent 是终态事件。text/model/input_tokens/output_tokens/steps/finish
// 均不带 omitempty，为 0/空串也会出现键名，方便消费方稳定 parse。
// finish 为 stop（正常完成）或 max_iterations（撞迭代上限，text 为空）。
type resultEvent struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Steps        int    `json:"steps"`
	Finish       string `json:"finish"`
}

// errorEvent 是终态错误事件。
type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ToolUseWriter 返回一个 OnToolUse 回调：每次调用把工具名与参数写成一条
// NDJSON tool_use 事件到 w。错误契约见 OnToolUse。
func ToolUseWriter(w io.Writer) OnToolUse {
	enc := json.NewEncoder(w)
	return func(name, input string) error {
		return enc.Encode(toolUseEvent{Type: "tool_use", Name: name, Input: input})
	}
}

func EmitResult(w io.Writer, result Result, model string) error {
	return json.NewEncoder(w).Encode(resultEvent{
		Type:         "result",
		Text:         result.Text,
		Model:        model,
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		Steps:        result.Steps,
		Finish:       result.Finish,
	})
}

func EmitError(w io.Writer, msg string) error {
	return json.NewEncoder(w).Encode(errorEvent{Type: "error", Message: msg})
}
