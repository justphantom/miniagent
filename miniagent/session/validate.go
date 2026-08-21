package session

import (
	"errors"
	"fmt"

	"github.com/justphantom/miniagent/miniagent"
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

