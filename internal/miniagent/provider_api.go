package miniagent

import "context"

type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
	Tools     []Tool
	// ThinkingLevel is the request-side thinking level (off/minimal/low/medium/high/xhigh/max).
	// An empty string or ThinkingOff is not written to the wire. The concrete field name / value mapping is
	// overridden by Thinking (for cross-provider compatibility).
	ThinkingLevel string
	// Thinking overrides the default wire field name (reasoning_effort) and the level value mapping; nil uses the default.
	Thinking *ThinkingMapping
	// Stream decides whether buildChatBody emits stream:true. It is forced internally by DoStream (true) /
	// prepareDo (false) — callers should call DoStream (streaming) or Do (non-streaming) directly and not set
	// Stream by hand; it remains exported only because the LLM interface contract requires passing a struct.
	Stream bool
}

type Response struct {
	Text      string
	Reasoning string // reasoning chain (reasoning_content / reasoning), fed back into history as needed
	// ReasoningState is provider-opaque reasoning wire state. Run persists it on the assistant message so the
	// next request can replay it without keeping provider-side conversation state.
	ReasoningState string
	ToolCalls      []ToolCall
	Usage          Usage
	FinishReason   string // stop|length|tool_calls|content_filter|null; non-stop means the answer was truncated/filtered
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Doer is the minimal capability of "send a non-streaming chat request". The compaction summary call only
// needs this (summarizeMiddle). Abstracting it from a concrete *ChatClient into an interface lets compaction
// attach any provider that can Do.
type Doer interface {
	Do(ctx context.Context, req Request) (Response, error)
}

// LLM is the provider interface Run depends on: Do (non-streaming) + DoStream (streaming). The core decouples
// from the concrete provider through this — callers may attach any implementation (OpenAI-compatible,
// Anthropic-native, local, mock) with zero changes to the core loop. Embedding Doer reuses the Do contract.
// This is the core's only dependency seam on external providers (besides tools/compaction/event hooks).
type LLM interface {
	Doer
	DoStream(ctx context.Context, req Request, onDelta func(Delta) error) (Response, error)
}

// ThinkingOff is the "off" sentinel for the thinking level: both an empty string and this value mean the
// thinking field is not written to the wire. The other levels (minimal/low/medium/high/xhigh/max) are
// validated by CLI/config and passed through to the wire.
const ThinkingOff = "off"

// ThinkingMapping lets a provider explicitly declare the wire name of the thinking field (e.g. openai's
// reasoning_effort) and the level value mapping (for cross-provider compatibility). Pinned rule: when
// thinking is enabled the provider must declare {field,map}, and the wire must go through the mapping.
// It is a core wire contract (referenced by Request.Thinking / LoopConfig.Thinking), so it stays in core
// and is not moved out with the config subpackage.
type ThinkingMapping struct {
	Field string            `json:"field"`
	Map   map[string]string `json:"map,omitempty"`
}

// Delta is the streaming increment pushed to the consumer (the onDelta callback parameter of LLM.DoStream).
// It is a core streaming contract (referenced by the LLM interface), so it stays in core; SSE parsing lives
// in internal/provider/openai.
type Delta struct {
	Kind DeltaKind
	Text string
}
