package miniagent

import "fmt"

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
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning,omitempty"`
	// ReasoningState carries a provider-opaque JSON array of reasoning output items. Responses uses it to
	// replay encrypted reasoning across function-call rounds; other providers ignore it. Compaction may clear
	// it only as a unit because encrypted state cannot be truncated safely.
	ReasoningState string     `json:"reasoning_state,omitempty"`
	ToolCalls      []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID     string     `json:"tool_call_id,omitempty"`
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

// ValidateToolPairing validates that assistant.tool_calls and tool messages are paired one-to-one;
// a break would be rejected by the endpoint with a 400, so it is intercepted up front.
// Lives in core because it is a pure message-structure validation (no persistence or I/O),
// shared by compaction (compactWithSummary) and session (LoadSession).
func ValidateToolPairing(msgs []Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("message %d: tool_call id %q duplicated", i+1, tc.ID)
				}
				pending[tc.ID] = true
			}
		case RoleTool:
			if !pending[m.ToolCallID] {
				return fmt.Errorf("message %d: tool message's tool_call_id %q has no matching assistant tool_call", i+1, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d assistant tool_call(s) missing matching tool result", len(pending))
	}
	return nil
}

// SessionMeta is the metadata first line of the session jsonl (type=session), facilitating session
// listing and multi-provider provenance. LLMRequests records the cumulative count of LLM requests.
// Lives in core because it is shared by session (persistence) and event (NDJSON output).
type SessionMeta struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Model       string `json:"model"`
	Workdir     string `json:"workdir"`
	Provider    string `json:"provider"`
	Created     string `json:"created"`
	LLMRequests int    `json:"llm_requests,omitempty"`
}
