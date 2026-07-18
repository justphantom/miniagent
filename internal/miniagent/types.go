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
	Tools     []ToolSpec
}

// ToolSpec declares one tool to the LLM (OpenAI function-calling schema).
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
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

// Tool is one agent tool the LLM may call.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Call        func(ctx context.Context, args string) ToolResult
}

// ToolResult is the outcome of one tool call.
type ToolResult struct {
	Output  string
	IsError bool
}

// SignalKind tags what a Signal reports.
type SignalKind string

const (
	SignalToolUse    SignalKind = "tool_use"
	SignalToolResult SignalKind = "tool_result"
)

// Signal is one out-of-band event the loop fires.
type Signal struct {
	Kind    SignalKind
	Name    string
	Input   string
	Output  string
	IsError bool
}

// EmitFunc receives out-of-band signals from the loop.
type EmitFunc func(sig Signal) error

// Result is what loop.Run returns.
type Result struct {
	Text  string
	Usage Usage
	Steps int
	// NewMessages 是本轮 Run 期间新增到对话历史的消息（含 assistant 工具调用、
	// tool 结果、最终 assistant 文本），供持久化层 append。命名 NewMessages
	// 而非 History，是为了避免被误读为"全量历史"。
	NewMessages []Message
	// Incomplete 为 true 表示因达到 maxIterations 上限而终止，
	// 此刻没有最终 Text，但 Usage 与 NewMessages 仍应被消费以避免 token 浪费。
	Incomplete bool
}

// LoopConfig carries the per-turn LLM parameters.
type LoopConfig struct {
	Model         string
	System        string
	MemoryContext string
	MaxTokens     int
	Tools         []Tool
}
