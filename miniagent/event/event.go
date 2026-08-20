// Package event serializes the observation signals of the agent loop into NDJSON (one JSON object per line) written to stdout.
// This is an external plugin for the miniagent core's "I/O convention" — the core loop itself produces no output format,
// it only notifies via LoopHooks callbacks; consumers that want NDJSON events hook up the Emit*/ToolUseWriter here,
// otherwise they get a pure library agent with no output.
package event

import (
	"encoding/json"
	"io"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
	"github.com/justphantom/miniagent/text"
)

// sessionEventType is the type discriminator for session events / the jsonl first-line metadata (same value as in the session package).
const sessionEventType = "session"

// toolUseEvent is the NDJSON event emitted for each tool call.
type toolUseEvent struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input string `json:"input"`
}

// resultEvent is the terminal event. text/model/input_tokens/output_tokens/steps/finish
// all omit omitempty, so even 0/empty-string values still produce keys, allowing consumers to parse stably.
// compacted/thinking_downgraded are the same machine-contract booleans: always present, false default.
type resultEvent struct {
	Type               string `json:"type"`
	Text               string `json:"text"`
	Model              string `json:"model"`
	InputTokens        int    `json:"input_tokens"`
	OutputTokens       int    `json:"output_tokens"`
	Steps              int    `json:"steps"`
	LLMRequests        int    `json:"llm_requests"`
	Finish             string `json:"finish"`
	Compacted          bool   `json:"compacted"`
	ThinkingDowngraded bool   `json:"thinking_downgraded"`
}

type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// EmitToolUse writes a tool_use event (tool name + raw JSON arguments). To emit events during offline replay just call it directly;
// at runtime it is wrapped by ToolUseWriter into an OnToolUse hook triggered before tool execution.
func EmitToolUse(w io.Writer, name, input string) error {
	return json.NewEncoder(w).Encode(toolUseEvent{Type: "tool_use", Name: name, Input: input})
}

// ToolUseWriter returns an OnToolUse callback: each invocation writes the tool name and arguments as an
// NDJSON tool_use event to w. Error contract per OnToolUse.
func ToolUseWriter(w io.Writer) miniagent.OnToolUse {
	return func(name, input string) error {
		return EmitToolUse(w, name, input)
	}
}

// EmitResult writes the terminal result event. compacted/thinking_downgraded mirror
// Result.Compacted/ThinkingDowngraded: consumers can detect "this round summarized" /
// "thinking was dropped to a downgrade" without parsing the transcript.
func EmitResult(w io.Writer, result miniagent.Result, model string) error {
	return json.NewEncoder(w).Encode(resultEvent{
		Type:               "result",
		Text:               result.Text,
		Model:              model,
		InputTokens:        result.Usage.InputTokens,
		OutputTokens:       result.Usage.OutputTokens,
		Steps:              result.Steps,
		LLMRequests:        result.LLMRequests,
		Finish:             result.Finish,
		Compacted:          result.Compacted,
		ThinkingDowngraded: result.ThinkingDowngraded,
	})
}

// EmitError writes the terminal error event.
func EmitError(w io.Writer, msg string) error {
	return json.NewEncoder(w).Encode(errorEvent{Type: "error", Message: msg})
}

// EmitSession writes a session event (the first NDJSON stream entry, type=session). When -save-session creates a new session
// it is emitted before Run, with the same structure as the session jsonl first-line metadata (id/model/workdir/provider/created),
// so consumers can programmatically capture session metadata from the first stdout line and continue to the next round.
func EmitSession(w io.Writer, meta session.SessionMeta) error {
	if meta.Type == "" {
		meta.Type = sessionEventType
	}
	return json.NewEncoder(w).Encode(meta)
}

// modelEvent is the NDJSON event for -list-models output (one available model per line).
// provider/model are separate fields: the model id itself can contain '/', so the text line "provider/model_id" cannot be reliably split.
type modelEvent struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// EmitModel writes a model event.
func EmitModel(w io.Writer, provider, model string) error {
	return json.NewEncoder(w).Encode(modelEvent{Type: "model", Provider: provider, Model: model})
}

// maxToolResultEventChars is the character limit for the output in tool_result events: the event gives consumers a summary,
// the full result still enters the history via trimForHistory for LLM refill. Decoupled from the history truncation limit.
const maxToolResultEventChars = 2000

// toolResultEvent is the result event after tool execution. exit_code is set by exec-backed tools
// (shell/git/go/npm/golangci-lint; pointer, omitted when nil) and only when the result carries a
// trustworthy code — other tools have no exit-code semantics, and validation/denied/timeout results
// of exec-backed tools never ran a command, so emitting the zero value there would read as
// is_error:true + exit_code:0 ("succeeded") simultaneously.
type toolResultEvent struct {
	Type      string `json:"type"` // tool_result
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	IsError   bool   `json:"is_error"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

// execBackedTools 报告哪些工具有真实退出码语义（非零退出 = 命令结论而非工具故障）。
var execBackedTools = map[string]bool{
	"shell": true,
}

// EmitToolResult writes a tool_result event. output is truncated to maxToolResultEventChars;
// only exec-backed tools emit exit_code (other tools' ExitCode is a semantically-empty zero value).
// An IsError result is admitted only when ExitCode>0: exitAwareResult et al. set ExitCodeNotSet (-1)
// on error paths, and validation-time rejections (denied option, decode failure) never execute a
// command at all — the field is omitted instead of asserting a bogus 0.
func EmitToolResult(w io.Writer, name, callID string, r miniagent.ToolResult) error {
	out := text.Truncate(r.Output, maxToolResultEventChars, "…[tool_result truncated]")
	ev := toolResultEvent{
		Type:      "tool_result",
		Name:      name,
		CallID:    callID,
		Output:    out,
		Truncated: len([]rune(r.Output)) > maxToolResultEventChars,
		IsError:   r.IsError,
	}
	if execBackedTools[name] && (!r.IsError || r.ExitCode > 0) {
		ec := r.ExitCode
		ev.ExitCode = &ec
	}
	return json.NewEncoder(w).Encode(ev)
}

// deltaEvent is a streaming incremental event (type is text_delta or reasoning_delta).
type deltaEvent struct {
	Type string `json:"type"`
	Step int    `json:"step"`
	Text string `json:"text"`
}

// EmitDelta writes a delta event. kind determines type: text→text_delta, reasoning→reasoning_delta;
// unknown kind produces no output (defensive).
func EmitDelta(w io.Writer, step int, kind miniagent.DeltaKind, text string) error {
	var t string
	switch kind {
	case miniagent.DeltaText:
		t = "text_delta"
	case miniagent.DeltaReasoning:
		t = "reasoning_delta"
	default:
		return nil
	}
	return json.NewEncoder(w).Encode(deltaEvent{Type: t, Step: step, Text: text})
}
