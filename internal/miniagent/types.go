package miniagent

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

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

// Tool is one agent tool the LLM may call. Name/Description/Parameters
// 即 OpenAI function-calling schema 三要素，序列化由 wire.go 完成。
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	// ResultLimit 是该工具结果进入历史消息的字符上限（<=0 用 maxToolResultInHistory
	// 默认）。read/edit 等代码类工具设高限，避免截断丢准确性。不参与工具 schema 序列化。
	ResultLimit int
	// SplitTruncate 为 true 时，工具结果按「头 N/4 + 尾 3N/4」分段截断（中间用省略标记连接），
	// 而非默认的 head-only。shell/grep/script 类工具的关键结论常在尾部（编译/测试错误汇总、
	// 命中上限提示），纯前截断会丢掉最该让模型看到的诊断信息、逼模型重跑（反而多烧 token）。
	// read/edit 等带行号的代码类工具保持 head-only（false）：前截断才符合「分段读大文件」语义。
	// 不参与工具 schema 序列化（与 ResultLimit 同属入历史策略，非 LLM 可见参数）。
	SplitTruncate bool
	// Call 加 json:"-"：防未来误 json.Marshal(Tool) 时报 unsupported type。
	Call func(ctx context.Context, args string) ToolResult `json:"-"`
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

// exitCodeNotSet 标记 shell 命令未产生有效退出码（超时或启动失败），与正常退出的
// 0（成功）/N（命令退出码）区分，供消费方识别「命令没真正跑完」。
const exitCodeNotSet = -1

type ToolResult struct {
	Output  string
	IsError bool
	// ExitCode 仅 shell 工具有意义；非 shell 工具不设置（零值 0），事件层按工具名
	// 决定是否输出该字段（见 A4 tool_result 事件）。
	ExitCode int
}

// OnToolUse 是工具执行前的回调：name 为工具名，input 为原始 JSON 参数。
// 返回 error 会沿调用链上抛到 Run——当下游不可写（stdout 管道被消费者提前
// 关闭）时立即终止循环，避免继续烧 token。传 nil 表示不通知。
type OnToolUse func(name, input string) error

// DeltaKind 标识 LLM 流式增量的种类，由 OnDelta 回调携带（流式模式启用后）。
type DeltaKind string

const (
	DeltaText      DeltaKind = "text"
	DeltaReasoning DeltaKind = "reasoning"
)

// LoopHooks 是 Run 在循环各点回调消费方的钩子集合，所有字段可 nil（不通知）。
// 这是 agent 核心的开放缝口：BeforeLLM/AfterLLM 让一切上下文策略（压缩、记忆、RAG 注入、
// 用量记账、溢出检测）外挂实现，核心循环本身不做任何上下文管理。
type LoopHooks struct {
	// BeforeLLM 每步调 LLM 前触发：可改写发给 LLM 的消息视图、提交持久化增量、累加用量、
	// 标记压缩。nil=透传（极简模式：核心不做上下文管理，原样发送 transcript）。
	// 这是上下文管理/压缩/记忆/RAG 注入的唯一缝口。
	BeforeLLM func(ctx context.Context, in StepInput) (StepOutput, error)
	// AfterLLM 每步 LLM 响应后触发，供观察用量、判定静默溢出、记账。nil=不通知。
	AfterLLM func(ctx context.Context, step int, resp Response) error
	// OnBudget 每步 LLM 响应累计用量（含零 usage 时的本地估算 fallback）后触发，拿到当前 step 与
	// 累计 total。返回 error（典型 ErrBudgetExceeded）→ 核心止循环（error 直接上抛，走熔断退出码）。
	// nil=核心内置预算判定（LoopConfig.MaxTotalTokens + 零 usage 本地估算 fallback）。预算成为可换策略：
	// 调用方经此钩子外挂自定义预算/熔断逻辑，核心不内置特定预算策略。
	OnBudget func(step int, total Usage) error
	// OnToolUse 工具执行前通知；返回 error 沿链上抛到 Run 终止循环（下游管道关闭时）。
	// 返回哨兵 ErrToolDenied（loop.go 定义）时仅拒绝该工具、不终止循环。
	OnToolUse func(name, input string) error
	// OnToolResult 工具执行后通知，透传 ToolResult（含 ExitCode / IsError）。
	OnToolResult func(name, callID string, r ToolResult) error
	// ShapeToolResult 工具执行后、结果入历史前触发，返回该 tool 消息的 content。返回空串=核心用
	// 内置默认成型（trimForHistory 截断 + 可选落盘）。返回 error 沿链上抛终止循环（下游管道关闭时），
	// 核心按 OnToolResult 同款路径为剩余 calls 补占位 tool 消息保配对。仅改 content，不可改 role/
	// tool_call_id——配对不变量由核心保证。nil=内置默认成型。工具结果成型（截断/落盘/RAG 摘要等）
	// 经此钩子外挂，核心不内置特定成型策略。
	ShapeToolResult func(name, callID string, step int, r ToolResult) (string, error)
	// OnDelta LLM 流式增量；非流式模式不触发。
	OnDelta func(step int, kind DeltaKind, text string) error
}

// StepInput 是 BeforeLLM 的入参：当前运行 transcript（只读意图）+ step + 请求级 System/Tools。
// BeforeLLM 据此决定本轮发给 LLM 的消息视图（可压缩、可注入、可原样透传）。
type StepInput struct {
	Step   int
	Msgs   []Message
	System string
	Tools  []Tool
}

// StepOutput 是 BeforeLLM 的回参。View 必填（发给 LLM 的消息）；其余可选，为核心副作用。
type StepOutput struct {
	// View 是本轮发给 LLM 的消息（必填；透传时 = 输入 Msgs）。
	View []Message
	// Commit=true 时核心把运行 transcript 替换为 View（压缩场景：收缩后即新 transcript）；
	// false 时核心保留原 transcript、仅本轮发 View（记忆/RAG 注入场景：注入不进 transcript）。
	Commit bool
	// Persist 额外追加到本轮持久化 delta（如压缩生成的 summary 消息）。
	// 带 Kind 的持久化消息会替换 newMsgs 中同 Kind 的旧条目（如多次压缩只留最新 summary）。
	Persist []Message
	// ExtraUsage 累加进本轮 Result.Usage（如摘要 LLM 调用的 token），非 nil 即生效。
	ExtraUsage *Usage
	// Compacted=true 标记本轮发生压缩，核心据此置 Result.Compacted（交互层据此 rewrite session）。
	Compacted bool
}

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
	// NewMessages 是本轮 Run 新增的消息（不含 History）：main 据此 append-only
	// 追加到 session jsonl，避免每次重写全量。出错轮可能为空/不完整，main 不落盘。
	NewMessages []Message
	// Compacted 标记本轮是否触发过摘要压缩（compactWithSummary 成功）。交互/入口层据此
	// 决定是否 rewrite session 文件——append-only 落盘的 newMsgs 含被屏障的旧 summary 与
	// 被压中段，长会话需机会性 rewrite 真正丢弃（审查 P2 session 文件永不压缩）。
	Compacted bool
	// ThinkingDowngraded 标记本轮 callLLM 是否发生过 thinking 降级。交互层据此清
	// baseCfg.ThinkingLevel，避免下一轮重传原值再撞一次 400（审查 P2 thinking 跨轮固化）。
	ThinkingDowngraded bool
}

const (
	finishStop          = "stop"
	finishMaxIterations = "max_iterations"
	// finishLength 是 provider 在 output 撞 max_tokens 或输入撑满 context 无生成空间时返回的
	// finish_reason（§P1-C 静默溢出 Case 3 判据）。
	finishLength = "length"
)

// ErrBudgetExceeded 由 Run 在累计 token（输入+输出）超过 LoopConfig.MaxTotalTokens
// 时返回。走 error 路径（CLI 退出码 1），调用方可 errors.Is 判定熔断 vs 真故障。
var ErrBudgetExceeded = errors.New("miniagent: token budget exceeded")

// ErrContextLength 由 HTTPClient 在端点返回 context 超限的 400 时返回。Run 据此
// 做一次历史收紧重试（见 trimHistoryForContext）；调用方亦可 errors.Is 判定。
var ErrContextLength = errors.New("miniagent: context length exceeded")

// ErrThinkingUnsupported 由 HTTPClient 在端点返回疑似 thinking 参数不被支持的 400 时返回。
// callLLM 据此一次性去 thinking 字段重试（审查 v2 #7）；误判无害（重试仍失败则上抛）。
var ErrThinkingUnsupported = errors.New("miniagent: thinking parameter unsupported")

// ErrToolDenied 由 OnToolUse 返回表示拒绝执行该工具（如危险命令未获确认）。
// handleToolCalls 据此跳过该工具（回填拒绝结果）、不终止循环；其他 error 仍终止。
var ErrToolDenied = errors.New("miniagent: tool denied by caller")

// LoopConfig 是 Run 的核心配置——只含 agent 循环本体所需：模型、系统提示、工具、历史、限额、流式、思考。
// 一切上下文策略（压缩窗口、保留轮数、摘要模板、真实用量、溢出检测…）已移出，经 LoopHooks.BeforeLLM/AfterLLM
// 外挂（见 NewCompaction）。这使得 Run 成为一个极简、无策略的 ReAct 核心调用方按需组装能力。
type LoopConfig struct {
	Model          string
	System         string
	SummaryRequest string // 撞迭代上限时注入的总结请求（内部引导消息，不持久化）；空用内置默认。
	MaxTokens      int
	Tools          []Tool
	// History 是本轮之前的会话历史，按序拼在新 user prompt 之前。Run 不修改其内容。
	History []Message
	// MaxIterations 覆盖单轮 LLM 调用上限；<=0 用 maxIterations 默认值。
	MaxIterations int
	// MaxTotalTokens 单轮累计 token（输入+输出）上限；<=0 不限。超限 Run 返回
	// ErrBudgetExceeded（走 error 事件 + 退出码 1）。
	MaxTotalTokens int
	// Stream 为 true 时 callLLM 走流式（DoStream），增量经 LoopHooks.OnDelta 推出；
	// 默认 false（非流式 Do）。
	Stream bool
	// ThinkingLevel / Thinking 透传到每次 callLLM 的 Request（思考级别 + 供应商映射）。
	ThinkingLevel string
	Thinking      *ThinkingMapping
	// MaxToolResultChars 是 tool 结果入历史的默认字符上限（兜底 Tool.ResultLimit；<=0 用内置默认）。
	MaxToolResultChars int
	// MaxParallelTools 是单步并行工具上限（<=0 用内置默认）。
	MaxParallelTools int
	// ToolOutputDir 非空时启用「工具输出落盘 + path 回读」：超 limit 的工具全文写入
	// <ToolOutputDir>/tool_<step>_<callID>_<n>.txt，历史 Content 改为预览+绝对路径提示。
	// 空=禁用（保留 trimForHistory 一次性硬截断）。
	ToolOutputDir string
	// ToolOutputRetention 是落盘文件保留时长；Run 启动时机会性清理更早文件。<=0 用 7d。
	ToolOutputRetention time.Duration
}

// CompactionOptions 是 NewCompaction 的参数——把原散布在 LoopConfig 的上下文压缩策略集中到一处，
// 让压缩成为可插拔的「外挂」而非核心配置。<=0/空 的字段在引擎内回落内置默认（见 context.go）。
type CompactionOptions struct {
	// Chat 是摘要压缩用的 client（可与主 chat 不同 provider）；nil 时调用方须自行注入 Summarize，
	// 否则 NewCompaction 用它构造 summarizeMiddle 回调。
	Chat *ChatClient
	// MaxTokens 是主请求的单次输出上限，供 isUsageOverflow 判定静默溢出（对标原 cfg.MaxTokens）。
	MaxTokens int
	// ContextWindow 是模型 context 上限（tokens）；<=0 关闭主动压缩（仅保留 ErrContextLength 被动重试）。
	ContextWindow int
	// Model 是主模型 id；CompactionModel 空时回落此值做摘要。
	Model string
	// CompactionModel 是摘要专用模型 id（可跨 provider）；空回落 Model。
	CompactionModel string
	System          string
	Tools           []Tool
	KeepRecent      int // compactWithSummary 保留的最近轮数（<=0 用内置默认）。
	// KeepReasoning/KeepToolArgs/KeepReasoningChars 是主动裁剪参数（P1/P4/P7，<=0/0 用内置默认）。
	KeepReasoning        int
	KeepToolArgs         int
	KeepReasoningChars   int
	SummarizerPrompt     string // 非空则全量 override 摘要 system prompt。
	SummaryMaxChars      int
	PreserveRecentTokens int
	UseRealUsage         bool
	// Auto 控制静默用量溢出检测（AfterLLM 采真实 usage 判溢出、置下步 Force 压缩）；false=关闭。
	Auto     bool
	Reserved int // 从 ContextWindow 预留的 token 缓冲（<=0 回落内置默认）。
	// SessionID 透传给 OnCompacting（CompactingInput.SessionID），空串兼容无 session。
	SessionID string
	// OnCompacting 是每次摘要前的钩子（注入 context / 一次性替换 summarizerPrompt），nil=不启用。
	OnCompacting CompactingHook
	Logger       *slog.Logger
}

// ThinkingOff 是思考级别的「关闭」哨兵：空串与它都表示不向 wire 写入思考字段。
// 其余级别（minimal/low/medium/high/xhigh/max）由 CLI/config 校验取值，wire 透传。
const ThinkingOff = "off"

// 权限模式（审查 v3 需求 §4）：default=薄软约束（写工具限 workdir、shell 拒 sudo/su），
// auto=无限制。default 不构成安全边界（shell 可 cd/绝对路径越界，写工具可符号链接逃逸）。
const (
	ModeDefault = "default"
	ModeAuto    = "auto"
)
