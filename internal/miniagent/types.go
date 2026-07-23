package miniagent

import "context"

// Message is one chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one LLM-requested tool invocation.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // raw JSON arguments string
}

// Request is the backend-agnostic call to the LLM.
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
	Call        func(ctx context.Context, args string) ToolResult
}

// Response is what the LLM returned for one Request.
type Response struct {
	Text         string
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string // stop|length|tool_calls|content_filter|null；非 stop 表示回答被截断/过滤
}

// Usage is the token accounting for one call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ToolResult is the outcome of one tool call.
type ToolResult struct {
	Output  string
	IsError bool
}

// OnToolUse 是工具执行前的回调：name 为工具名，input 为原始 JSON 参数。
// 返回 error 会沿调用链上抛到 Run——当下游不可写（stdout 管道被消费者提前
// 关闭）时立即终止循环，避免继续烧 token。传 nil 表示不通知。
type OnToolUse func(name, input string) error

// Result is what loop.Run returns.
type Result struct {
	Text  string
	Usage Usage
	Steps int
}

// LoopConfig carries the per-turn LLM parameters.
type LoopConfig struct {
	Model     string
	System    string
	MaxTokens int
	Tools     []Tool
}
