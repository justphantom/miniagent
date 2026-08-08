package miniagent

import (
	"context"
	"time"
)

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
	// Call 是工具的实际执行函数。实现须尊重 ctx：ctx 取消须及时返回，否则 runToolsParallel 的
	// wg.Wait 会挂死、Run 不响应 SIGINT（Go 无法强制终止 goroutine，核心不内置超时——由各工具自实现，
	// 如 shell 的 shellTimeout）。加 json:"-"：防误 json.Marshal(Tool) 报 unsupported type。
	Call func(ctx context.Context, args string) ToolResult `json:"-"`
}

// ExitCodeNotSet 标记 shell 命令未产生有效退出码（超时或启动失败），与正常退出的
// 0（成功）/N（命令退出码）区分，供消费方识别「命令没真正跑完」。
const ExitCodeNotSet = -1

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

// LoopHooks 是 Run 在循环各点回调消费方的钩子集合，所有字段可 nil（不通知；极简模式）。
// 这是 agent 核心的开放缝口：BeforeLLM/AfterLLM（上下文视图/观察）、OnBudget（用量记账+预算判定）、
// OnLLMError（失败恢复）、ShapeToolResult（工具结果成型）让一切上下文/用量/成型策略外挂实现，
// 核心循环本身不做任何上下文管理、估算、预算判定或错误恢复——仅做工具注册/上下文拼接/调 LLM/
// 执行工具/退出循环五件事。默认钩子实现见 NewDefault* 工厂（cmd 层组装即可复用原内置行为）。
type LoopHooks struct {
	// BeforeLLM 每步调 LLM 前触发：可改写发给 LLM 的消息视图、提交持久化增量、累加用量、
	// 标记压缩。nil=透传（极简模式：核心不做上下文管理，原样发送 transcript）。
	// 这是上下文管理/压缩/记忆/RAG 注入的唯一缝口。
	BeforeLLM func(ctx context.Context, in StepInput) (StepOutput, error)
	// AfterLLM 每步 LLM 响应后触发，供观察用量、判定静默溢出、记账。nil=不通知。
	AfterLLM func(ctx context.Context, step int, resp Response) error
	// OnBudget 每步 LLM 响应后触发：核心已把真实 usage 累加进 total，本钩子负责零 usage 时的本地
	// 估算 fallback（向 total 累加）与预算判定。返回 error（典型 ErrBudgetExceeded）→ 核心止循环
	// （error 直接上抛，走熔断退出码）。nil=核心不估算不判定（极简模式：仅累加真实 usage，不熔断）。
	// 预算成为可换策略：调用方经此钩子外挂自定义预算/熔断逻辑，核心不内置特定预算策略。
	// 默认实现见 NewDefaultOnBudget（承载原 EstimateTokens fallback + MaxTotalTokens 判定）。
	OnBudget func(ctx context.Context, step int, in BudgetInput, total *Usage) error
	// OnLLMError 单步 LLM 调用失败后触发，供外挂错误恢复（典型：ErrContextLength 时收紧历史重试一次）。
	// 返回 recoveredMsgs 非 nil → 核心用其替换运行 transcript 后重试本次调用；retry=false 或钩子 nil →
	// 核心不做恢复，error 直接上抛终止循环；返回的 err 非 nil 同样上抛。重试仅一次（核心不递归）。
	// 这是 LLM 失败路径的唯一缝口（BeforeLLM/AfterLLM 都在成功路径）。默认实现见 NewDefaultOnLLMError。
	OnLLMError func(ctx context.Context, step int, msgs []Message, callErr error) (recoveredMsgs []Message, retry bool, retErr error)
	// OnToolUse 工具执行前通知；返回 error 沿链上抛到 Run 终止循环（下游管道关闭时）。
	// 返回哨兵 ErrToolDenied（errors.go 定义）时仅拒绝该工具、不终止循环。
	OnToolUse func(name, input string) error
	// OnToolResult 工具执行后通知，透传 ToolResult（含 ExitCode / IsError）。同一步内多个 tool_call 并行执行，
	// 本回调在全部完成后按 tool_call 顺序串行通知——非「每工具完成即通知」，实时性受最慢工具制约。
	OnToolResult func(name, callID string, r ToolResult) error
	// ShapeToolResult 工具执行后、结果入历史前触发，返回该 tool 消息的 content。返回空串=核心透传
	// 原始 Output（零成型）；返回非空串=核心用其覆盖 content。返回 error 沿链上抛终止循环（下游管道
	// 关闭时），核心按 OnToolResult 同款路径为剩余 calls 补占位 tool 消息保配对。仅改 content，不可改
	// role/tool_call_id——配对不变量由核心保证。nil=核心透传原文（极简模式）；默认成型（trimForHistory
	// 截断 + 可选落盘）由 NewDefaultShapeToolResult 承载、cmd 层组装时挂载，核心不内置特定成型策略。
	ShapeToolResult func(name, callID string, step int, r ToolResult) (string, error)
	// OnDelta LLM 流式增量；非流式模式不触发。返回 error 会中止流——经 callLLMOnce 上抛到 Run 终止循环
	// （非 ErrThinkingUnsupported，不触发降级）。下游管道关闭时用此提前终止，避免继续烧 token。
	OnDelta func(step int, kind DeltaKind, text string) error
}

// StepInput 是 BeforeLLM 的入参：当前运行 transcript（只读意图）+ step + 请求级 System/Tools。
// BeforeLLM 据此决定本轮发给 LLM 的消息视图（可压缩、可注入、可原样透传）。
//
// Msgs 与运行 transcript 共享底层数组（仅 slice header 拷贝），钩子须只读——原地改 Msgs[i] 字段会
// 污染 Run 的 transcript。需改写应经 StepOutput.View/Persist 返回，由核心折叠回状态。
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

// BudgetInput 是 OnBudget 的请求侧上下文：本轮发给 LLM 的消息视图 + 请求级 System/Tools + 响应。
// 供钩子做零 usage 本地估算 fallback（EstimateTokens 计 toSend/system/tools；estimateResponseTokens 计 resp）。
type BudgetInput struct {
	ToSend []Message
	System string
	Tools  []Tool
	Resp   Response
}

type Result struct {
	Text  string
	Usage Usage
	// Steps 是本轮已记账 usage 的 LLM 调用数（usage 在 recordStepUsage 累加：未记的失败路径计
	// step-1、已记的计 step；总结额外调用计 step+1）。出错/取消路径同此语义。仅观察用，非逻辑依赖。
	Steps int
	// LLMRequests 是本轮实际发给 LLM 端点的请求次数（含 thinking 降级重试、OnLLMError 收紧重试、
	// 总结步），不含 provider 层透明重试（503 退避）与压缩步的独立 LLM 调用。与 Steps 不同：
	// Steps 只数 usage 已记账的步，LLMRequests 数实际 HTTP 请求（降级/重试使两者常不等）。
	LLMRequests int
	// Finish 是终止原因：FinishStop（模型给出最终文本）或
	// FinishMaxIterations（撞迭代上限，Text 为空）。出错返回时为空串。
	Finish string
	// Messages 是截至返回时的全量 transcript（History + 本轮新增），
	// 所有 return 路径（含出错、撞 maxIterations）都带回，供会话持久化。
	Messages []Message
	// NewMessages 是本轮 Run 新增的消息（不含 History）：main 据此 append-only
	// 追加到 session jsonl，避免每次重写全量。Run 经 defer 保证出错/取消路径也带回 NewMessages，
	// 且 tool_call↔tool_result 配对由 handleToolCalls 的 fillPlaceholderTail 补全完整——main 在
	// 出错/取消路径亦调用 saveSession 落盘，救回失败前已执行的部分供 resume（仅失败那一步 LLM 本就
	// 无产出，天然不在此列）。
	NewMessages []Message
	// Compacted 标记本轮是否触发过摘要压缩（BeforeLLM 钩子置 StepOutput.Compacted，核心据此回填）。交互/入口层据此
	// 决定是否 rewrite session 文件——append-only 落盘的 newMsgs 含被屏障的旧 summary 与
	// 被压中段，长会话需机会性 rewrite 真正丢弃（审查 P2 session 文件永不压缩）。
	Compacted bool
	// ThinkingDowngraded 标记本轮 callLLMWithDowngrade 是否发生过 thinking 降级。交互层据此清
	// baseCfg.ThinkingLevel，避免下一轮重传原值再撞一次 400（审查 P2 thinking 跨轮固化）。
	ThinkingDowngraded bool
}

const (
	FinishStop          = "stop"
	FinishMaxIterations = "max_iterations"
)

// LoopConfig 是 Run 的配置。字段分两类：循环本体字段（Model/System/Tools/History/MaxIterations/
// MaxTokens/Stream/Thinking…）核心 Run 直接读；策略字段（MaxTotalTokens/MaxToolResultChars/
// ToolOutputDir/ToolOutputRetention）核心 Run 不读，仅作配置载体，由调用方提取喂给 NewDefault* 钩子
// 工厂（见 cmd/miniagent）。一切上下文/用量/成型策略经 LoopHooks 外挂，Run 是极简、无策略的 ReAct 核心。
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
	// MaxTotalTokens 单轮累计 token（输入+输出）上限；<=0 不限。策略配置载体——核心 Run 不读，
	// 由 NewDefaultOnBudget 据此判定熔断（超限返回 ErrBudgetExceeded，走 error 事件 + 退出码 1）。
	MaxTotalTokens int
	// Stream 为 true 时 callLLMOnce 走流式（DoStream），增量经 LoopHooks.OnDelta 推出；
	// 默认 false（非流式 Do）。
	Stream bool
	// ThinkingLevel / Thinking 透传到每次 callLLMOnce 的 Request（思考级别 + 供应商映射）。
	ThinkingLevel string
	Thinking      *ThinkingMapping
	// MaxToolResultChars 是 tool 结果入历史的默认字符上限（兜底 Tool.ResultLimit；<=0 用内置默认）。
	// 策略配置载体——核心 Run 不读，由 NewDefaultShapeToolResult 读取。
	MaxToolResultChars int
	// MaxParallelTools 是单步并行工具上限（<=0 用内置默认）。
	MaxParallelTools int
	// ToolOutputDir 非空时启用「工具输出落盘 + path 回读」：超 limit 的工具全文写入
	// <ToolOutputDir>/tool_<step>_<callID>_<n>.txt，历史 Content 改为预览+绝对路径提示。
	// 空=禁用（仅 trimForHistory 截断）。策略配置载体——核心 Run 不读，由 NewDefaultShapeToolResult 读取。
	ToolOutputDir string
	// ToolOutputRetention 是落盘文件保留时长；NewDefaultShapeToolResult 构造时机会性清理更早文件。<=0 用 7d。
	// 策略配置载体——核心 Run 不读。
	ToolOutputRetention time.Duration
}

// 权限模式（审查 v3 需求 §4）：default=薄软约束（写工具限 workdir、shell 拒 sudo/su），
// auto=无限制。default 不构成安全边界（shell 可 cd/绝对路径越界，写工具可符号链接逃逸）。
const (
	ModeDefault = "default"
	ModeAuto    = "auto"
)
