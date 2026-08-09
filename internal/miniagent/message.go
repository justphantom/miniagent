package miniagent

// Message role constants: loop/session/wire all match the same set of values; extracting constants
// guards against spelling drift.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// KindSummary marks a summary message (Message.Kind): structured recognition (used by
// applyCompactionBarrier / the compaction engine), replacing fragile content-prefix sniffing.
// role=user is legitimate and persistable. It is a core concept (shared by compaction and session), so it
// is defined here.
const KindSummary = "summary"

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// IsError marks whether the tool execution corresponding to a tool message failed (ToolResult.IsError).
	// Only meaningful for the tool role; the wire chatMessage does not carry this field (buildChatBody does
	// not emit it, so it never leaks to the LLM) — it is used only for session persistence and history
	// trimming (P8' success check: a later successful write/edit on the same path folds an earlier args).
	// omitempty keeps backward compatibility with old sessions (absent = zero value false = treated as success).
	IsError bool `json:"is_error,omitempty"`
	// Kind is a session-layer marker (e.g. KindSummary), used only for persistence and context-barrier
	// recognition; the wire chatMessage does not carry this field — buildChatBody builds it independently
	// and never leaks it to the LLM.
	Kind string `json:"kind,omitempty"`
	// Usage is the real billed usage of the LLM response corresponding to this assistant message; nil means
	// no usage (user/tool messages, or a response that carried no usage). It is used only for session
	// persistence and context usage estimation (estimateTokensFromUsage); the wire chatMessage does not
	// carry this field — buildChatBody builds it independently and never leaks it to the LLM.
	// omitempty keeps backward compatibility with old sessions (absent = nil = fall back to local estimation).
	Usage *Usage `json:"usage,omitempty"`
	// Ts is the Unix-millisecond timestamp when the message was produced, used by the "real usage staleness
	// guard" (see lastApplicableUsageIndex): if a prefix message with a newer Ts is inserted after some
	// assistant.usage (typically a summary), that usage no longer describes the current prefix and is stale.
	// omitempty keeps backward compatibility with old sessions; appendMsg auto-stamps Ts==0, so 0 reliably
	// means "not carried".
	Ts int64 `json:"ts,omitempty"`
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // raw JSON arguments string
}
