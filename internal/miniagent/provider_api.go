package miniagent

import "context"

type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
	Tools     []Tool
	// ThinkingLevel 是请求侧思考级别（off/minimal/low/medium/high/xhigh/max）。
	// 空串或 ThinkingOff 不写入 wire。具体字段名/取值映射由 Thinking 覆盖（跨供应商兼容）。
	ThinkingLevel string
	// Thinking 覆盖默认 wire 字段名（reasoning_effort）与级别取值映射；nil 用默认。
	Thinking *ThinkingMapping
	// Stream 决定 buildChatBody 是否生成 stream:true；由 prepareDo（false）/DoStream
	// （true）强制设置，Do/DoStream 行为据此确定，不暴露给调用方决策。
	Stream bool
}

type Response struct {
	Text         string
	Reasoning    string // 思考链（reasoning_content / reasoning），按需入历史回灌
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string // stop|length|tool_calls|content_filter|null；非 stop 表示回答被截断/过滤
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Doer 是「发一个非流式 chat 请求」的最小能力。压缩的摘要调用只需此（summarizeMiddle）。
// 把它从具体 *ChatClient 抽成接口，使压缩可挂任意能 Do 的 provider。
type Doer interface {
	Do(ctx context.Context, req Request) (Response, error)
}

// LLM 是 Run 依赖的 provider 接口：Do（非流式）+ DoStream（流式）。核心据此与具体 provider 解耦——
// 调用方可挂任意实现（OpenAI 兼容、Anthropic 原生、本地、mock），核心循环零改动。嵌入 Doer 复用 Do 契约。
// 这是核心对外部 provider 的唯一依赖缝口（除工具/压缩/事件钩子外）。
type LLM interface {
	Doer
	DoStream(ctx context.Context, req Request, onDelta func(Delta) error) (Response, error)
}

// ThinkingOff 是思考级别的「关闭」哨兵：空串与它都表示不向 wire 写入思考字段。
// 其余级别（minimal/low/medium/high/xhigh/max）由 CLI/config 校验取值，wire 透传。
const ThinkingOff = "off"

// Delta 是推给消费方的流式增量（LLM.DoStream 的 onDelta 回调参数）。
// 属核心流式契约（LLM 接口引用），故留 core；SSE 解析在 internal/provider/openai。
type Delta struct {
	Kind DeltaKind
	Text string
}
