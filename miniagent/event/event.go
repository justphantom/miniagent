// Package event serializes the observation signals of the agent loop into NDJSON (one JSON object per line) written to stdout.
// This is an external plugin for the miniagent core's "I/O convention" — the core loop itself produces no output format,
// it only notifies via LoopHooks callbacks; consumers that want NDJSON events hook up the Emit*/ToolUseWriter here,
// otherwise they get a pure library agent with no output.
package event

import (
	"encoding/json"
	"io"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/text"
)

// sessionEventType is the type discriminator for session events / the jsonl first-line metadata (same value as in the session package).
const sessionEventType = "session"

// toolUseEvent is the NDJSON event emitted for each tool call.
// Ts is a Unix-millisecond timestamp (omitted when 0): the WebUI renders per-message time,
// and replay passes the persisted message.Ts so historical messages show their original time.
type toolUseEvent struct {
	Type   string `json:"type"`
	Step   int    `json:"step"`
	Name   string `json:"name"`
	CallID string `json:"call_id"`
	Input  string `json:"input"`
	Ts     int64  `json:"ts,omitempty"`
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
	// Truncated is the stream-abort signal (R4): the text was cut by an upstream mid-stream abort,
	// not by the model finishing. Omit-if-zero — unlike the always-present keys above it is a new
	// field, so old consumers never see it unless the cut actually happened.
	Truncated bool  `json:"truncated,omitempty"`
	Ts        int64 `json:"ts,omitempty"`
}

// stamp resolves the optional Unix-ms timestamp argument: an explicit ts is preserved (replay
// carries the persisted message.Ts), otherwise the event is stamped at emission time.
func stamp(ts []int64) int64 {
	if len(ts) > 0 && ts[0] > 0 {
		return ts[0]
	}
	return text.NowMs()
}

type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// EmitToolUse writes a tool_use event (tool name + call id + raw JSON arguments). To emit events during offline replay just call it directly;
// at runtime it is wrapped by ToolUseWriter into an OnToolUse hook triggered before tool execution.
// ts is an optional Unix-millisecond timestamp (0 → stamp now); the WebUI renders per-message time.
func EmitToolUse(w io.Writer, step int, name, callID, input string, ts ...int64) error {
	return json.NewEncoder(w).Encode(toolUseEvent{Type: "tool_use", Step: step, Name: name, CallID: callID, Input: input, Ts: stamp(ts)})
}

// ToolUseWriter returns an OnToolUse callback: each invocation writes the tool name, call id and arguments as an
// NDJSON tool_use event to w. Error contract per OnToolUse.
func ToolUseWriter(w io.Writer) miniagent.OnToolUse {
	return func(step int, name, callID, input string) error {
		return EmitToolUse(w, step, name, callID, input)
	}
}

// EmitResult writes the terminal result event. compacted/thinking_downgraded mirror
// Result.Compacted/ThinkingDowngraded: consumers can detect "this round summarized" /
// "thinking was dropped to a downgrade" without parsing the transcript.
func EmitResult(w io.Writer, result miniagent.Result, model string, ts ...int64) error {
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
		Truncated:          result.Truncated,
		Ts:                 stamp(ts),
	})
}

// EmitError writes the terminal error event.
func EmitError(w io.Writer, msg string) error {
	return json.NewEncoder(w).Encode(errorEvent{Type: "error", Message: msg})
}

// stopEvent is the terminal stop event: the turn was canceled (explicit stop / context
// cancellation) after partial execution; the session jsonl still holds the executed part.
type stopEvent struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// EmitStop writes the terminal stop event. Cancellation used to return silently with no
// terminal event — NDJSON consumers keying on result/error to detect completion saw a clean
// EOF and misreported it as a dropped connection.
func EmitStop(w io.Writer, reason string) error {
	return json.NewEncoder(w).Encode(stopEvent{Type: "stop", Reason: reason})
}

// UserPromptType is the replay-only event type carrying a persisted user message.
// The runtime turn stream never emits user events (see replay.go), so this type
// appears only in replay/refresh streams; the webui renders it as a user message.
const UserPromptType = "user_prompt"

// userPromptEvent is the replay-only event for a persisted user message.
type userPromptEvent struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Ts   int64  `json:"ts,omitempty"`
}

// EmitUserPrompt writes a user_prompt event (replay only). ts is a Unix-ms timestamp
// (0 → stamp now); replay passes the persisted message.Ts so history shows original time.
func EmitUserPrompt(w io.Writer, text string, ts ...int64) error {
	return json.NewEncoder(w).Encode(userPromptEvent{Type: UserPromptType, Text: text, Ts: stamp(ts)})
}

// EmitSession writes a session event (the first NDJSON stream entry, type=session). When -save-session creates a new session
// it is emitted before Run, with the same structure as the session jsonl first-line metadata (id/model/workdir/provider/created),
// so consumers can programmatically capture session metadata from the first stdout line and continue to the next round.
func EmitSession(w io.Writer, meta miniagent.SessionMeta) error {
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
	Step      int    `json:"step"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	IsError   bool   `json:"is_error"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Ts        int64  `json:"ts,omitempty"`
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
func EmitToolResult(w io.Writer, step int, name, callID string, r miniagent.ToolResult, ts ...int64) error {
	out := text.Truncate(r.Output, maxToolResultEventChars, "…[tool_result truncated]")
	ev := toolResultEvent{
		Type:      "tool_result",
		Step:      step,
		Name:      name,
		CallID:    callID,
		Output:    out,
		Truncated: len([]rune(r.Output)) > maxToolResultEventChars,
		IsError:   r.IsError,
		Ts:        stamp(ts),
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
	Ts   int64  `json:"ts,omitempty"`
}

// EmitDelta writes a delta event. kind determines type: text→text_delta, reasoning→reasoning_delta;
// unknown kind produces no output (defensive). ts is an optional Unix-ms timestamp (0 → stamp now).
func EmitDelta(w io.Writer, step int, kind miniagent.DeltaKind, text string, ts ...int64) error {
	var t string
	switch kind {
	case miniagent.DeltaText:
		t = "text_delta"
	case miniagent.DeltaReasoning:
		t = "reasoning_delta"
	default:
		return nil
	}
	return json.NewEncoder(w).Encode(deltaEvent{Type: t, Step: step, Text: text, Ts: stamp(ts)})
}

type stepUsageEvent struct {
	Type         string `json:"type"`
	Step         int    `json:"step"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	ToolCalls    int    `json:"tool_calls"`
}

// EmitStepUsage writes a step_usage event carrying per-step incremental token counts.
func EmitStepUsage(w io.Writer, step, inTokens, outTokens, toolCalls int) error {
	return json.NewEncoder(w).Encode(stepUsageEvent{Type: "step_usage", Step: step, InputTokens: inTokens, OutputTokens: outTokens, ToolCalls: toolCalls})
}
