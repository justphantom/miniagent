package miniagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// maxSessionBytes 是 session 文件的大小上限：超大文件既撑内存，又会被完整
// 拼进下次请求的 messages 烧 token 或触发端点 400。对齐 maxChatBodyBytes。
const maxSessionBytes = 4 << 20

// LoadSession 读取 path 处的会话历史（JSON 数组的 []Message）。文件不存在
// 返回 (nil, nil)，等同无历史的新会话。文件存在但损坏（JSON 非法、role
// 未知、tool 消息缺 tool_call_id、超过大小上限）返回 error：调用方应直接
// 报错退出，不静默丢弃历史。
func LoadSession(path string) ([]Message, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSessionBytes {
		return nil, fmt.Errorf("session 文件 %q 超过大小上限 %d 字节", path, maxSessionBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("session 文件 %q 不是合法 JSON：%w", path, err)
	}
	for i, m := range msgs {
		if err := validateSessionMessage(m); err != nil {
			return nil, fmt.Errorf("session 文件 %q 第 %d 条消息非法：%w", path, i, err)
		}
	}
	if err := validateToolPairing(msgs); err != nil {
		return nil, fmt.Errorf("session 文件 %q：%w", path, err)
	}
	return msgs, nil
}

func validateSessionMessage(m Message) error {
	switch m.Role {
	case "user", "assistant":
		return nil
	case "tool":
		if m.ToolCallID == "" {
			return errors.New("tool 消息缺少 tool_call_id")
		}
		return nil
	default:
		return fmt.Errorf("未知 role %q", m.Role)
	}
}

// validateToolPairing 校验 assistant.tool_calls 与 tool 消息的一一配对。
// 配对断裂（手改/截断的 session）会被 OpenAI 兼容端点 400 拒绝，且报错指向
// LLM 端而非 session 文件，这里提前拦截并指明位置。
func validateToolPairing(msgs []Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("第 %d 条：tool_call id %q 重复", i, tc.ID)
				}
				pending[tc.ID] = true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				return fmt.Errorf("第 %d 条：tool 消息的 tool_call_id %q 没有对应的 assistant tool_call", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d 个 assistant tool_call 缺少对应 tool 结果", len(pending))
	}
	return nil
}

// SaveSession 把完整 transcript 原子写回 path（temp+rename），权限 0o600
// （对话内容属敏感数据）。思考内容不入上下文由类型层保证：Message 没有
// reasoning 字段，序列化结果天然不含思考内容，无需剥离逻辑。
func SaveSession(path string, msgs []Message) error {
	if len(msgs) == 0 {
		// nil 会被序列化成 "null"，回读得到 nil，"空会话"与"新会话"无法
		// 区分。当前调用链不会产生空 transcript，拒写是防御缺口。
		return errors.New("session: 空 transcript 拒绝写盘")
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}
