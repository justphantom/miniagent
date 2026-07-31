package miniagent

import "context"

// 消息 role 常量：loop/session/wire 多处匹配同一组取值，抽常量防拼写漂移。
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // raw JSON arguments string
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
	Tools     []Tool
}

// Tool is one agent tool the LLM may call. Name/Description/Parameters
// 即 OpenAI function-calling schema 三要素，序列化由 wire.go 完成。
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	// Call 加 json:"-"：防未来误 json.Marshal(Tool) 时报 unsupported type。
	Call        func(ctx context.Context, args string) ToolResult `json:"-"`
}

type Response struct {
	Text         string
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string // stop|length|tool_calls|content_filter|null；非 stop 表示回答被截断/过滤
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type ToolResult struct {
	Output  string
	IsError bool
}

// OnToolUse 是工具执行前的回调：name 为工具名，input 为原始 JSON 参数。
// 返回 error 会沿调用链上抛到 Run——当下游不可写（stdout 管道被消费者提前
// 关闭）时立即终止循环，避免继续烧 token。传 nil 表示不通知。
type OnToolUse func(name, input string) error

type Result struct {
	Text  string
	Usage Usage
	Steps int
	// Finish 是终止原因：finishStop（模型给出最终文本）或
	// finishMaxIterations（撞迭代上限，Text 为空）。出错返回时为空串。
	Finish string
	// Messages 是截至返回时的全量 transcript（History + 本轮新增），
	// 所有 return 路径（含出错、撞 maxIterations）都带回，供会话持久化。
	Messages []Message
}

const (
	finishStop          = "stop"
	finishMaxIterations = "max_iterations"
)

// LoopConfig carries the per-turn LLM parameters.
type LoopConfig struct {
	Model     string
	System    string
	MaxTokens int
	Tools     []Tool
	// History 是本轮之前的会话历史，按序拼在新 user prompt 之前。
	// Run 不修改其内容。
	History []Message
}
