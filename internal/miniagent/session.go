package miniagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// LoadSession 读取 path 处的会话历史（JSON 数组的 []Message）。文件不存在
// 返回 (nil, nil)，等同无历史的新会话。文件存在但损坏（JSON 非法、role
// 未知、tool 消息缺 tool_call_id）返回 error：调用方应直接报错退出，不
// 静默丢弃历史。
func LoadSession(path string) ([]Message, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
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

// SaveSession 把完整 transcript 原子写回 path（temp+rename），权限 0o600
// （对话内容属敏感数据）。思考内容不入上下文由类型层保证：Message 没有
// reasoning 字段，序列化结果天然不含思考内容，无需剥离逻辑。
func SaveSession(path string, msgs []Message) error {
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}
