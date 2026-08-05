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

// EmitSession 写一条 session 事件（NDJSON 流首条，type=session）。-save-session 新建会话时
// 在 Run 之前 emit，结构同 session jsonl 首行 metadata（id/model/workdir/provider/created），
// 供消费方从 stdout 第一行程序化捕获会话元数据接续下轮（替代 stderr 文本 grep）。
func EmitSession(w io.Writer, meta SessionMeta) error {
	if meta.Type == "" {
		meta.Type = sessionTypeSession
	}
	return json.NewEncoder(w).Encode(meta)
}

// modelEvent 是 -list-models 输出的 NDJSON 事件（每行一个可用模型）。
// provider/model 分离字段：model id 本身可含 '/'，文本行 "provider/model_id" 无法可靠拆分。
type modelEvent struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// EmitModel 写一条 model 事件。
func EmitModel(w io.Writer, provider, model string) error {
	return json.NewEncoder(w).Encode(modelEvent{Type: "model", Provider: provider, Model: model})
}

// maxToolResultEventChars 是 tool_result 事件里 output 的字符上限：事件给消费方看
// 概要，完整结果仍经 trimForHistory 入历史回灌 LLM。与历史裁剪上限解耦。
const maxToolResultEventChars = 2000

// toolResultEvent 是工具执行后的结果事件。exit_code 仅 shell 工具设置（指针，
// nil 时不输出）——其他工具无退出码语义，避免零值 0 被误读为「成功」。
type toolResultEvent struct {
	Type      string `json:"type"` // tool_result
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	IsError   bool   `json:"is_error"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

// EmitToolResult 写一条 tool_result 事件。output 截断到 maxToolResultEventChars；
// 仅 shell 工具输出 exit_code（其他工具 ExitCode 是无语义的零值）。
func EmitToolResult(w io.Writer, name, callID string, r ToolResult) error {
	out := truncate(r.Output, maxToolResultEventChars, "…[tool_result 已截断]")
	ev := toolResultEvent{
		Type:      "tool_result",
		Name:      name,
		CallID:    callID,
		Output:    out,
		Truncated: len([]rune(r.Output)) > maxToolResultEventChars,
		IsError:   r.IsError,
	}
	if name == "shell" {
		ec := r.ExitCode
		ev.ExitCode = &ec
	}
	return json.NewEncoder(w).Encode(ev)
}

// deltaEvent 是流式增量事件（type 为 text_delta 或 reasoning_delta）。
type deltaEvent struct {
	Type string `json:"type"`
	Step int    `json:"step"`
	Text string `json:"text"`
}

// EmitDelta 写一条增量事件。kind 决定 type：text→text_delta，reasoning→reasoning_delta；
// 未知 kind 不输出（防御）。
func EmitDelta(w io.Writer, step int, kind DeltaKind, text string) error {
	var t string
	switch kind {
	case DeltaText:
		t = "text_delta"
	case DeltaReasoning:
		t = "reasoning_delta"
	default:
		return nil
	}
	return json.NewEncoder(w).Encode(deltaEvent{Type: t, Step: step, Text: text})
}
