package session

import (
	"errors"
	"fmt"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// ValidateSessionID validates id against a whitelist: only Latin letters, digits, and hyphens are allowed.
// Path separators / dots / spaces etc. are forbidden so that the id serves only as the file name body (the .jsonl
// extension is appended by ResolveSessionPath), ruling out path traversal and extension injection.
// An empty id is rejected outright (tightened export contract: callers mostly check arg=="" first, but this
// function must not treat an empty string as valid).
func ValidateSessionID(id string) error {
	if id == "" {
		return errors.New("session id is empty")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("session id %q contains illegal character %q (only Latin letters, digits, and - are allowed)", id, r)
		}
	}
	return nil
}

func validateSessionMessage(m miniagent.Message) error {
	switch m.Role {
	case miniagent.RoleUser, miniagent.RoleAssistant:
		return nil
	case miniagent.RoleTool:
		if m.ToolCallID == "" {
			return errors.New("tool message missing tool_call_id")
		}
		return nil
	default:
		return fmt.Errorf("unknown role %q", m.Role)
	}
}

// ValidateToolPairing validates that assistant.tool_calls and tool messages are paired one-to-one; a break would
// be rejected by the endpoint with a 400, so it is intercepted up front with the position indicated.
func ValidateToolPairing(msgs []miniagent.Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case miniagent.RoleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("message %d: tool_call id %q duplicated", i+1, tc.ID)
				}
				pending[tc.ID] = true
			}
		case miniagent.RoleTool:
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
