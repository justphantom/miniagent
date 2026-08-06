package miniagent

// 消息 role 常量：loop/session/wire 多处匹配同一组取值，抽常量防拼写漂移。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// KindSummary 标记 summary 消息（Message.Kind）：结构化识别（applyCompactionBarrier / 压缩引擎用），
// 替代脆弱的内容前缀嗅探。role=user 合法可持久化。属核心概念（压缩与 session 共享），故定义于此。
const KindSummary = "summary"

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// IsError 标记 tool 消息对应工具执行是否失败（ToolResult.IsError）。仅 tool 角色有意义；
	// wire 的 chatMessage 不含此字段（buildChatBody 不输出，不泄漏给 LLM），仅 session 持久化
	// 与历史裁剪（P8' 成功判定：同 path 更晚的成功 write/edit 才折叠更早的 args）用。
	// omitempty 向后兼容旧 session（缺失=零值 false=视为成功）。
	IsError bool `json:"is_error,omitempty"`
	// Kind 是 session 层标记（如 KindSummary），仅持久化与上下文屏障识别用；
	// wire 的 chatMessage 不含此字段——buildChatBody 独立构造，绝不泄漏给 LLM。
	Kind string `json:"kind,omitempty"`
	// Usage 是该 assistant 消息对应 LLM 响应的真实计费用量；nil 表示无用量（user/tool 消息或
	// 未携带用量的响应）。仅 session 持久化与上下文用量估算（estimateTokensFromUsage）用，
	// wire 的 chatMessage 不含此字段——buildChatBody 独立构造，绝不泄漏给 LLM。
	// omitempty 向后兼容旧 session（缺失=nil=回落本地估算）。
	Usage *Usage `json:"usage,omitempty"`
	// Ts 是消息产生时的 Unix 毫秒时间戳，供「真实 usage 防陈旧」判定（见 lastApplicableUsageIndex）：
	// 若某 assistant.usage 之后插入了 Ts 更新的前缀消息（典型：摘要），该 usage 不再描述当前前缀，应失效。
	// omitempty 向后兼容旧 session；appendMsg 对 Ts==0 自动打戳，故 0 可靠表示「未承载」。
	Ts int64 `json:"ts,omitempty"`
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // raw JSON arguments string
}
