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
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
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
	Text    string
	Usage   Usage
	Steps   int
	History []Message
}

// LoopConfig carries the per-turn LLM parameters.
type LoopConfig struct {
	Model         string
	System        string
	MemoryContext string
	MaxTokens     int
	Tools         []Tool
}

func spec(name, desc string, params map[string]any) ToolSpec {
	return ToolSpec{Name: name, Description: desc, Parameters: params}
}
