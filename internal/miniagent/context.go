package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
)

const (
	// summaryMaxChars 是摘要的字符上限（初值，按实测调，设计 §15.4）。
	summaryMaxChars = 5000
	// summaryMaxTokens 限制摘要请求的输出 token 数：端点默认上限远超 summaryMaxChars
	// 对应的 token 量，不限会先生成超长输出再被字符截断，白烧 token（审查 P3-1）。
	summaryMaxTokens = 1024
)

// summaryMaxTokensOverride 允许配置覆盖内置默认；用 atomic 保护并发 Set/Get，防 -race。
var summaryMaxTokensOverride atomic.Int64

// SetSummaryMaxTokens 覆盖摘要最大 token 数；测试用，正常流程由 Resolve 调用。
func SetSummaryMaxTokens(n int) {
	if n > 0 {
		summaryMaxTokensOverride.Store(int64(n))
	}
}

func getSummaryMaxTokens() int {
	if v := summaryMaxTokensOverride.Load(); v > 0 {
		return int(v)
	}
	return summaryMaxTokens
}

// summarizerSystem 是压缩专用 system prompt：要求把历史压成一段受限摘要，保留关键事实。
// 默认值通过 LoopConfig.SummarizerPrompt 覆盖；调用方需传入 maxChars（summaryMaxChars）。
const summarizerSystem = "你是会话压缩器。把以下对话历史压缩为一段不超过 %d 字符的中文摘要，保留关键事实、决策、文件改动与未决问题，不要复述全部对话，不要输出多余解释。"

// ContextBudget 是 FitHistory 的单一参数包：上下文窗口、保留轮数、摘要提示与模型、
// 以及把中段压成摘要的可注入回调（解耦 context.go 与 HTTPClient，便于测试注入假摘要）。
// System/Tools 用于 estimateTokens 估算（计入 system prompt 与工具 schema 的固定开销）。
type ContextBudget struct {
	ContextWindow int // 0 = 不主动压缩
	KeepRecent    int // <=0 回落 contextKeepRecent
	// KeepReasoning 是主动 reasoning 清理时保留的最近 assistant 消息条数（<=0 回落 contextKeepReasoning）。
	// FitHistory 在 fitted 结果上清空非最近 N 条 assistant 的 Reasoning（P1）。
	KeepReasoning int
	// KeepToolArgs 是主动 tool_call args 压缩时保留的最近 assistant 条数（<=0 回落 contextKeepToolArgs）。
	// FitHistory 在 fitted 结果上压缩非最近 N 条 assistant 的 write/edit 大 args（P4）。
	KeepToolArgs int
	// KeepReasoningChars 是保留窗口内单条 Reasoning 的字符上限（P7）。FitHistory 在 stripStaleReasoning
	// 之后对保留的最近 N 条 assistant 的超长 Reasoning 做头尾分段（threshold rune）。0=回落
	// contextKeepReasoningChars；<0=关闭（不截）；>0=自定义阈值。
	KeepReasoningChars int
	SummarizerPrompt   string
	CompactionModel    string // 空则回落 Model
	Model              string
	System             string
	Tools              []Tool
	// Summarize 把中段 middle 压成摘要文本（已截断到 maxChars）+ 该次调用 Usage。
	// Run 注入：func(ctx, model, summarizerPrompt, middle) → summarizeMiddle(...)。
	Summarize func(ctx context.Context, model, summarizerPrompt string, middle []Message) (string, Usage, error)
}

// FitHistory 是 Run 阶段 2 的单一入口：超 window 80% 时摘要中段，失败/无中段回落有损压缩，
// 仍超裁到最近轮，再超报错（审查 v2 #8 + v3 #4）。返回：
//   - out：处理后的 msgs（可能含新 summary）；
//   - summary：本轮生成的 KindSummary 消息（summary.Kind=="" 表示未生成，调用方据此判 summarized）；
//   - summarized：是否成功摘要压缩（Run 据此设 result.Compacted 并把 summary 插入 newMsgs）；
//   - usage：摘要调用的 token 用量（Run 累加进 total，MaxTotalTokens 预算含摘要调用）；
//   - err：即使有损裁剪后仍超 window 时返回，Run 应终止避免循环烧请求。
//
// 不触碰 newMsgs——持久化层的 summary 插入/去重由 Run 用返回的 summary 完成（见 insertSummaryIntoNewMsgs）。
func FitHistory(ctx context.Context, msgs []Message, budget ContextBudget, logger *slog.Logger) (out []Message, summary Message, summarized bool, usage Usage, err error) {
	keepReasoning := budget.KeepReasoning
	if keepReasoning <= 0 {
		keepReasoning = contextKeepReasoning
	}
	keepToolArgs := budget.KeepToolArgs
	if keepToolArgs <= 0 {
		keepToolArgs = contextKeepToolArgs
	}
	keepReasoningChars := budget.KeepReasoningChars
	if keepReasoningChars == 0 {
		keepReasoningChars = contextKeepReasoningChars
	}
	// keepReasoningChars < 0 → truncateKeptReasoning 内部 threshold<=0 原样返回（关闭）。
	if budget.ContextWindow <= 0 || estimateTokens(msgs, budget.System, budget.Tools) <= budget.ContextWindow*4/5 {
		// P1/P4/P6/P7/P8'/P9b/P11：reasoning 与 tool_call args 的主动清理 + 保留窗口内超长 reasoning 体积裁剪 +
		// 跨消息去重/折叠，均为默认策略，即使未超窗/窗口未知也执行——旧 Reasoning 是思考模型下隐性 token 大户
		// （P1 清条、P7 裁保留条体积）；write/edit 大 args 写成功后纯占位重发（P4 压前缀、P8' 被后续同 path 成功
		// 写入取代时整条折叠）；重复 read 结果（P6 按 path+offset 保留最后一次）、同义 shell command（P9b）压占位；
		// edit/write 成功后同 path 的更早 read 结果（P11）折叠。均与 tool 配对无关，清/压/去重不丢可见事实（正文/
		// tool_calls ID 不动）。无可处理项时各自原样返回，零开销。
		out := stripStaleReasoning(msgs, keepReasoning)
		out = truncateKeptReasoning(out, keepReasoning, keepReasoningChars)
		out = stripStaleToolArgs(out, keepToolArgs)
		out = dedupReadResults(out, keepToolArgs)
		out = foldStaleReadResults(out, keepToolArgs)
		out = foldStaleWriteEditArgs(out, keepToolArgs)
		out = dedupShellCommands(out, keepToolArgs)
		return out, Message{}, false, Usage{}, nil
	}
	keepRecent := budget.KeepRecent
	if keepRecent <= 0 {
		keepRecent = contextKeepRecent
	}
	fitted, sm, sumUsage, serr := compactWithSummary(ctx, budget, msgs, keepRecent)
	if serr != nil {
		if logger != nil {
			logger.Warn("summarize 失败，回落有损压缩", "error", serr)
		}
	} else if sm.Kind != KindSummary && logger != nil {
		logger.Warn("无中段可摘要，有损压缩")
	}
	summarized = sm.Kind == KindSummary
	out = fitted
	if !summarized {
		out = compactHistory(msgs, keepRecent)
	}
	// P1/P4/P6/P7/P8'/P9b/P11：在 fitted 结果上主动清空非最近 N 条 assistant 的 Reasoning（P1）、裁保留窗口内超长
	// reasoning 体积（P7）、压缩 write/edit 大 args（P4）、read 结果按 path+offset 去重（P6）、被后续同 path 成功
	// 写入取代的 write/edit args 折叠（P8'）、同义 shell command 去重（P9b）、被后续同 path 成功写入取代的旧 read
	// 结果折叠（P11）。放在 window 检查前——清理后 token 估计更低，更可能免于触发 trimRecentRounds/终止报错。
	// 中段（已并入 summary）不经此处；最近 N 条 assistant 的 reasoning 与写入 args 保留，供模型延续当前上下文
	// （P7 仅压超长中段，两端保留）。
	out = stripStaleReasoning(out, keepReasoning)
	out = truncateKeptReasoning(out, keepReasoning, keepReasoningChars)
	out = stripStaleToolArgs(out, keepToolArgs)
	out = dedupReadResults(out, keepToolArgs)
	out = foldStaleReadResults(out, keepToolArgs)
	out = foldStaleWriteEditArgs(out, keepToolArgs)
	out = dedupShellCommands(out, keepToolArgs)
	if estimateTokens(out, budget.System, budget.Tools) > budget.ContextWindow*4/5 {
		out = trimRecentRounds(out, keepRecent)
		if logger != nil {
			logger.Warn("仍超 window，裁到最近轮", "msgs", len(out))
		}
	}
	if estimateTokens(out, budget.System, budget.Tools) > budget.ContextWindow*4/5 {
		return out, sm, summarized, sumUsage, fmt.Errorf("history 超 context window（约 %d tokens）即使有损裁剪后仍超——终止以避免循环烧请求", estimateTokens(out, budget.System, budget.Tools))
	}
	return out, sm, summarized, sumUsage, nil
}

// applyCompactionBarrier 定位最新一条 Kind=="summary" 消息，返回它及之后的消息；之前的
// 旧历史（含更老 summary）不进 context，仍留 session 文件。无 summary 原样返回。
func applyCompactionBarrier(msgs []Message) []Message {
	for i := range slices.Backward(msgs) {
		if msgs[i].Kind == KindSummary {
			return msgs[i:]
		}
	}
	return msgs
}

// summarizeMiddle 调 LLM 把中段 msgs 压成一段摘要文本（不带 tools）。返回经 maxChars 截断的
// 摘要 + 该次调用的 Usage（供上游累加入预算）。复用 ChatClient.Do；调用方据 error 回落
// 有损压缩（审查 v2 #6）。summarizerPrompt 空时回落默认 summarizerSystem。
func summarizeMiddle(ctx context.Context, llm *ChatClient, model, summarizerPrompt string, maxChars int, msgs []Message) (string, Usage, error) {
	if len(msgs) == 0 {
		return "", Usage{}, errors.New("无中段可摘要")
	}
	system := summarizerSystem
	if summarizerPrompt != "" {
		system = summarizerPrompt
	}
	resp, err := llm.Do(ctx, Request{
		Model:     model,
		System:    fmt.Sprintf(system, maxChars),
		Messages:  msgs,
		MaxTokens: getSummaryMaxTokens(),
	})
	if err != nil {
		return "", Usage{}, err
	}
	return truncate(strings.TrimSpace(resp.Text), maxChars, "…[摘要已截断]"), resp.Usage, nil
}

// compactWithSummary 保留最早 1 轮 + 最近 keepRecent 轮，中段摘要为单条 KindSummary 消息。
// 返回 (out, summary, usage, err)：out 含新 summary；summary 是该消息（.Kind=="" 表示无中段/失败）；
// 中段配对断裂或摘要失败返回 error（调用方 FitHistory 回落 compactHistory）。无中段可摘返回
// (msgs, Message{}, Usage{}, nil)。不再接收 newMsgs——持久化插入由 Run 完成。
//
// 跨轮继承（P2-1）：上轮 LoadSession 带入的旧 summary 经 applyCompactionBarrier 落在 msgs
// 头，splitRounds 使其单独成 rounds[0]。middle 默认排除 rounds[0] → LLM 输入从未含旧
// summary 文本，新 summary 不继承远古历史；下轮 barrier 命中新 summary 后旧 summary 被丢弃，
// 其承载的历史永久失真。检测到该遗留 summary 时并入 middle 开头让 LLM 真正继承，并从 out 头
// 移除——否则新旧 summary 双存，下轮 splitRounds 又把新 summary 孤立进 rounds[0] 重演同一
// bug。首轮非 summary（正常 user 轮）维持原行为。
func compactWithSummary(ctx context.Context, budget ContextBudget, msgs []Message, keepRecent int) (out []Message, summary Message, usage Usage, err error) {
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs, Message{}, Usage{}, nil // 无中段可摘
	}
	middle := flatten(rounds[1 : len(rounds)-keepRecent])
	head := rounds[0]
	if len(head) == 1 && head[0].Kind == KindSummary {
		middle = append([]Message{head[0]}, middle...)
		head = nil
	}
	if len(middle) == 0 {
		return msgs, Message{}, Usage{}, nil
	}
	// 中段必须自洽配对：否则替换进 summary 会留下孤立的 tool_call/tool，续跑被端点 400。
	if err := validateToolPairing(middle); err != nil {
		return msgs, Message{}, Usage{}, fmt.Errorf("中段配对断裂，无法安全摘要：%w", err)
	}
	compModel := budget.CompactionModel
	if compModel == "" {
		compModel = budget.Model
	}
	text, sumUsage, err := budget.Summarize(ctx, compModel, budget.SummarizerPrompt, middle)
	if err != nil {
		return msgs, Message{}, Usage{}, err
	}
	summaryMsg := Message{Role: roleUser, Kind: KindSummary, Content: "[既往对话摘要]\n" + text}
	out = append([]Message{}, head...)
	out = append(out, summaryMsg)
	out = append(out, flatten(rounds[len(rounds)-keepRecent:])...)
	return out, summaryMsg, sumUsage, nil
}

// insertSummaryIntoNewMsgs 剔除 newMsgs 中已有的 KindSummary（单轮多次压缩去重——上一次写进
// newMsgs 的旧 summary 其对应中段已被本次新 summary 覆盖），再把 summary 前插使其排在
// user_prompt 之前（跨轮 barrier 命中最新 summary，否则本轮 user_prompt 会被一并屏障，使
// 「保留最近 N 轮」跨轮失效，审查 P1-1/P2）。
func insertSummaryIntoNewMsgs(newMsgs *[]Message, summary Message) {
	filtered := make([]Message, 0, len(*newMsgs)+1)
	for _, m := range *newMsgs {
		if m.Kind != KindSummary {
			filtered = append(filtered, m)
		}
	}
	*newMsgs = append([]Message{summary}, filtered...)
}

const systemOverheadTokens = 400

// envelopePerMsgTokens / envelopePerToolCallTokens 是请求信封的线性化 token 估算（信封对齐）。
// systemOverheadTokens 是固定 base；这两项随消息数与 tool_call 数线性增长——每条 message 的
// role/字段名/标点、每个 tool_call 嵌套的 {id,type,function{name,arguments}} 对象随会话累积，长 ReAct
// 会话远超 flat 400。原先单一常数系统性低估、压缩触发偏晚、更易撞 context_length_exceeded 白烧一次失败
// 往返。取值偏保守（略高估信封使压缩略早触发，优于撞墙往返）。
const (
	envelopePerMsgTokens      = 4
	envelopePerToolCallTokens = 20
)

// perToolSchemaTokens 粗估单个工具的 schema（name+description+parameters JSON）入请求的 token 数。
const perToolSchemaTokens = 60

// contextKeepRecent 是 compactHistory/compactWithSummary 默认保留的最近轮数。
// P3：从 6 下调到 4——稳态占用降一截，更早的轮次由 summary（已含关键事实）承载；配置钩子
// cfg.Run.ContextKeepRecent 仍在，长 ReAct 场景可调回更高值。
const contextKeepRecent = 4

// contextKeepReasoning 是主动 reasoning 清理默认保留的最近 assistant 消息条数（P1）。
// 1 = 仅保留最近一条 assistant 的思考链（当前推理上下文），更早的清空。
const contextKeepReasoning = 1

// contextKeepReasoningChars 是保留窗口内单条 Reasoning 的字符上限（P7，rune）。超过则做头尾分段
// （复用 truncateHeadTail 的头 1/4 + 尾 3/4），把思考模型超长 reasoning 的中段发散压掉、两端保留。
// 4000 rune ≈ 2000 token：仅 >4000 的超长思考链才截，绝大多数短 reasoning 零影响。0=未配置回落本默认；
// config run.context_keep_reasoning_chars 设负数关闭、正数自定义阈值。
const contextKeepReasoningChars = 4000

// contextKeepToolArgs 是主动 tool_call args 压缩默认保留的最近 assistant 条数（P4）。
// write/edit 的 content/old/new_string 写成功后已落盘或被替换，非最近 N 条 assistant 的这类大 args
// 压缩为前缀占位（仅改 context 侧拷贝，不动配对）。2 = 保留最近 2 条 assistant 的写入参数原文，
// 覆盖「正在改的文件」上下文窗口；高准确度场景可经 config run.context_keep_tool_args 调高。
const contextKeepToolArgs = 2

// toolArgsCompressThreshold / toolArgsKeepChars 是 P4 写入参数压缩的内置阈值：
// 字段超过 threshold（rune）才压缩，压成前 keepChars（rune）+ 省略标记；path 始终保留。
// 取值平衡：阈值过低会压短改动、损失模型连续性；keepChars 过低丢失文件头部上下文（如 import/包声明）。
const (
	toolArgsCompressThreshold = 600
	toolArgsKeepChars         = 200
)
