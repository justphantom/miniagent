package miniagent

import (
	"errors"
	"fmt"
)

// ValidateSessionID 白名单校验 id：仅允许拉丁字母、数字、连字符。禁路径分隔符/点/空格等，
// 使 id 只作文件名主体（.jsonl 扩展名由 ResolveSessionPath 补），杜绝路径穿越与扩展名注入。
func ValidateSessionID(id string) error {
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("session id %q 含非法字符 %q（仅允许拉丁字母、数字、-）", id, r)
		}
	}
	return nil
}

func validateSessionMessage(m Message) error {
	switch m.Role {
	case RoleUser, RoleAssistant:
		return nil
	case RoleTool:
		if m.ToolCallID == "" {
			return errors.New("tool 消息缺少 tool_call_id")
		}
		return nil
	default:
		return fmt.Errorf("未知 role %q", m.Role)
	}
}

// ValidateToolPairing 校验 assistant.tool_calls 与 tool 消息一一配对；断裂会被端点 400，提前拦截指明位置。
func ValidateToolPairing(msgs []Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("第 %d 条：tool_call id %q 重复", i+1, tc.ID)
				}
				pending[tc.ID] = true
			}
		case RoleTool:
			if !pending[m.ToolCallID] {
				return fmt.Errorf("第 %d 条：tool 消息的 tool_call_id %q 没有对应的 assistant tool_call", i+1, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d 个 assistant tool_call 缺少对应 tool 结果", len(pending))
	}
	return nil
}
