package miniagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// 单次 open + LimitReader：消除 Stat/ReadFile 之间的 TOCTOU，并把读取量硬封顶，
	// 防文件被并发替换为超大内容撑爆内存（maxSessionBytes 的原始意图）。
	data, err := io.ReadAll(io.LimitReader(f, maxSessionBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSessionBytes {
		return nil, fmt.Errorf("session 文件 %q 超过大小上限 %d 字节", path, maxSessionBytes)
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
	case roleUser, roleAssistant:
		return nil
	case roleTool:
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
		case roleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("第 %d 条：tool_call id %q 重复", i, tc.ID)
				}
				pending[tc.ID] = true
			}
		case roleTool:
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
// （对话内容属敏感数据）。思考内容（reasoning）随 Message.Reasoning 序列化落盘
// （与 Content 同级、对称），支持 reasoning 模型跨会话续跑；属敏感数据但文件已
// 0o600。若不希望落盘，需在调用前显式清零各 Message.Reasoning。
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
