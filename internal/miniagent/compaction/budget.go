// budget.go：预算估算、配置常量与 FitHistory 入口。

package compaction

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
)

const (
	// summaryMaxChars 是摘要的字符上限（初值，按实测调，设计 §15.4）。
	summaryMaxChars = 5000
	// summaryMaxTokens 限制摘要请求的输出 token 数：端点默认上限远超 summaryMaxChars
	// 对应的 token 量，不限会先生成超长输出再被字符截断，白烧 token（审查 P3-1）。
	summaryMaxTokens = 1024
)

// ContextBudget 是 FitHistory 的单一参数包：上下文窗口、保留轮数、摘要提示与模型、
// 以及把中段压成摘要的可注入回调（解耦 context.go 与 HTTPClient，便于测试注入假摘要）。
// System/Tools 用于 miniagent.EstimateTokens 估算（计入 system prompt 与工具 schema 的固定开销）。
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
	Tools              []miniagent.Tool
	// UseRealUsage 控制 estimateThreshold 是否优先采纳真实 usage（§P0-B）。false（kill-switch）
	// 直接用本地 miniagent.EstimateTokens，回落旧行为；true 时无可用真实 usage 亦自动回落本地估算。
	UseRealUsage bool
	// Force 为 true 时 FitHistory 跳过 miniagent.EstimateTokens 4/5 门控，直接进入 compactWithSummary+有损 fallback
	// 分支（§P1-B）。由 Run 在上一步真实 usage 命中 isUsageOverflow 时置位，让压缩基于「已证实的真实占用」触发。
	Force bool
	// PreserveRecentTokens 是 retainedTail 的 token 预算上界（§P1-E，移植 opencode preserve_recent_tokens）。
	// <=0=自动：floor(ContextWindow/tailBudgetFraction) clamp [min,max]；ContextWindow<=0 时返回 0 关闭，
	// 回落 KeepRecent 轮数模式（向后兼容老会话与无窗口配置）。
	PreserveRecentTokens int
	// Compacting 在每次摘要前触发（compactWithSummary 内、调 budget.Summarize 前），允许注入
	// context 或一次性替换 summarizerPrompt（镜像 opencode experimental.session.compacting，§P2）。
	// nil=不启用。Run 构造 budget 时从 LoopHooks.OnCompacting 桥接。
	Compacting CompactingHook
	// SessionID 经 budget 带入 compactWithSummary 作用域，供 applyCompactingHook 读（CompactingInput.SessionID）。
	SessionID string
	// Summarize 把中段 middle 压成摘要文本（已截断到 maxChars）+ 该次调用 miniagent.Usage。
	// previousSummary 非空时走 UPDATE 模式（保留旧摘要作锚点更新），由 compactWithSummary
	// 从遗留 miniagent.KindSummary 抽出经此参数下传。Run 注入：func(ctx, model, summarizerPrompt,
	// previousSummary, middle) → summarizeMiddle(...)。
	Summarize func(ctx context.Context, model, summarizerPrompt, previousSummary string, middle []miniagent.Message) (string, miniagent.Usage, error)
}

// FitHistory 是 Run 阶段 2 的单一入口：超 window 80% 时摘要中段，失败/无中段回落有损压缩，
// 仍超裁到最近轮，再超报错（审查 v2 #8 + v3 #4）。返回：
//   - out：处理后的 msgs（可能含新 summary）；
//   - summary：本轮生成的 miniagent.KindSummary 消息（summary.Kind=="" 表示未生成，调用方据此判 summarized）；
//   - summarized：是否成功摘要压缩（Run 据此设 result.Compacted 并把 summary 插入 newMsgs）；
//   - usage：摘要调用的 token 用量（Run 累加进 total，MaxTotalTokens 预算含摘要调用）；
//   - err：即使有损裁剪后仍超 window 时返回，Run 应终止避免循环烧请求。
//
// 不触碰 newMsgs——持久化层的 summary 插入/去重由 Run 用返回的 summary 完成（见 insertSummaryIntoNewMsgs）。
func FitHistory(ctx context.Context, msgs []miniagent.Message, budget ContextBudget, logger *slog.Logger) (out []miniagent.Message, summary miniagent.Message, summarized bool, usage miniagent.Usage, err error) {
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
	// §P0-B：阈值判定改用 estimateThreshold（优先真实 usage、回落本地估算），补 miniagent.EstimateTokens
	// 对缓存内容零感知的盲区，长会话不再系统性偏晚触发压缩。
	// §P1-B：Force=true（上一步真实 usage 命中 isUsageOverflow）时跳过估算门控，直接进 compactWithSummary
	// 分支，让压缩基于「已证实的真实占用」触发。Force 仅在 ContextWindow>0 时被置位，故不产生 ContextWindow<=0
	// 的非法 Force 路径；后续 trimRecentRounds/终止门控（:116/:122）仍用 ContextWindow，不受影响。
	if !budget.Force && (budget.ContextWindow <= 0 || estimateThreshold(msgs, budget.System, budget.Tools, budget.UseRealUsage) <= budget.ContextWindow*4/5) {
		// P1/P4/P6/P7/P8'/P9b/P11 主动裁剪（各阶段语义见 applyContextStrips；Debug level 记录节省 token）。
		out := applyContextStrips(ctx, msgs, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
		return out, miniagent.Message{}, false, miniagent.Usage{}, nil
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
	} else if sm.Kind != miniagent.KindSummary && logger != nil {
		logger.Warn("无中段可摘要，有损压缩")
	}
	summarized = sm.Kind == miniagent.KindSummary
	out = fitted
	if !summarized {
		out = compactHistory(msgs, keepRecent)
	}
	// P1/P4/P6/P7/P8'/P9b/P11 主动裁剪（语义见 applyContextStrips；放在 window 检查前——清理后 token 估计更低，
	// 更可能免于触发 trimRecentRounds/终止报错。中段已并入 summary 不经此处）。
	out = applyContextStrips(ctx, out, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
	if miniagent.EstimateTokens(out, budget.System, budget.Tools) > budget.ContextWindow*4/5 {
		out = trimRecentRounds(out, keepRecent)
		if logger != nil {
			logger.Warn("仍超 window，裁到最近轮", "msgs", len(out))
		}
	}
	if miniagent.EstimateTokens(out, budget.System, budget.Tools) > budget.ContextWindow*4/5 {
		return out, sm, summarized, sumUsage, fmt.Errorf("history 超 context window（约 %d tokens）即使有损裁剪后仍超——终止以避免循环烧请求", miniagent.EstimateTokens(out, budget.System, budget.Tools))
	}
	return out, sm, summarized, sumUsage, nil
}

// applyContextStrips 跑全部主动裁剪（P1/P4/P6/P7/P8'/P9b/P11），仅改 context 侧拷贝，供 FitHistory
// 未超窗/超窗两分支复用（原两处内联序列一致，抽此避免重复 + 统一可观测）。logger 为 Debug level
// （CLI -log-level debug）时，记录各阶段节省的 token（miniagent.EstimateTokens 差值）与 fit 前后总量，供 v11 §6
// 的「确实省了」运行时确认；Info level（默认）不算差值、零开销。各阶段语义见各 strip 函数 doc。
func applyContextStrips(ctx context.Context, msgs []miniagent.Message, keepReasoning, keepReasoningChars, keepToolArgs int, logger *slog.Logger, sys string, tools []miniagent.Tool) []miniagent.Message {
	dbg := logger != nil && logger.Enabled(ctx, slog.LevelDebug)
	strip := func(stage string, fn func([]miniagent.Message) []miniagent.Message, in []miniagent.Message) []miniagent.Message {
		if !dbg {
			return fn(in)
		}
		before := miniagent.EstimateTokens(in, sys, tools)
		o := fn(in)
		if after := miniagent.EstimateTokens(o, sys, tools); before > after {
			logger.Debug("context budget: strip saved",
				"stage", stage, "saved_tokens", before-after, "before_msgs", len(in), "after_msgs", len(o))
		}
		return o
	}
	out := strip("P1_reasoning", func(m []miniagent.Message) []miniagent.Message { return stripStaleReasoning(m, keepReasoning) }, msgs)
	out = strip("P7_reasoningTrunc", func(m []miniagent.Message) []miniagent.Message {
		return truncateKeptReasoning(m, keepReasoning, keepReasoningChars)
	}, out)
	out = strip("P4_toolArgs", func(m []miniagent.Message) []miniagent.Message { return stripStaleToolArgs(m, keepToolArgs) }, out)
	out = strip("P6_dedupRead", func(m []miniagent.Message) []miniagent.Message { return dedupReadResults(m, keepToolArgs) }, out)
	out = strip("P11_foldRead", func(m []miniagent.Message) []miniagent.Message { return foldStaleReadResults(m, keepToolArgs) }, out)
	out = strip("P8p_foldWriteEdit", func(m []miniagent.Message) []miniagent.Message { return foldStaleWriteEditArgs(m, keepToolArgs) }, out)
	out = strip("P9b_dedupShell", func(m []miniagent.Message) []miniagent.Message { return dedupShellCommands(m, keepToolArgs) }, out)
	if dbg {
		logger.Debug("context budget: fit done",
			"before_tokens", miniagent.EstimateTokens(msgs, sys, tools), "after_tokens", miniagent.EstimateTokens(out, sys, tools), "msgs", len(out))
	}
	return out
}

// preserveRecentTokens 解析 retainedTail token 预算上界（移植 opencode preserveRecentBudget，§P1-E）：
// budget.PreserveRecentTokens>0 直接返回；否则 floor(budget.ContextWindow/tailBudgetFraction)
// clamp [minPreserveRecentTokens, maxPreserveRecentTokens]；budget.ContextWindow<=0 返回 0（关闭 → 纯轮数模式）。
// 无副作用、纯函数、易测。
func preserveRecentTokens(budget ContextBudget) int {
	if budget.PreserveRecentTokens > 0 {
		return budget.PreserveRecentTokens
	}
	if budget.ContextWindow <= 0 {
		return 0
	}
	t := budget.ContextWindow / tailBudgetFraction
	if t < minPreserveRecentTokens {
		return minPreserveRecentTokens
	}
	if t > maxPreserveRecentTokens {
		return maxPreserveRecentTokens
	}
	return t
}

// estimateRoundTokens 估算单轮的边际 token（content+reasoning+args+信封），不计 system/schema 全局开销
// ——这两项是请求级常量，在 tail 总量里只计一次，故逐轮累加时必须用边际估算（否则每轮重复计 400+schema，
// 系统性高估、tail 恒不达标）。供 selectTailByTokens 累加。
func estimateRoundTokens(round []miniagent.Message) int {
	return miniagent.EstimateTokens(round, "", nil) - miniagent.SystemOverheadTokens
}

// insertSummaryIntoNewMsgs 剔除 newMsgs 中已有的 miniagent.KindSummary（单轮多次压缩去重——上一次写进
// newMsgs 的旧 summary 其对应中段已被本次新 summary 覆盖），再把 summary 前插使其排在
// user_prompt 之前（跨轮 barrier 命中最新 summary，否则本轮 user_prompt 会被一并屏障，使
// 「保留最近 N 轮」跨轮失效，审查 P1-1/P2）。
func insertSummaryIntoNewMsgs(newMsgs *[]miniagent.Message, summary miniagent.Message) {
	filtered := make([]miniagent.Message, 0, len(*newMsgs)+1)
	for _, m := range *newMsgs {
		if m.Kind != miniagent.KindSummary {
			filtered = append(filtered, m)
		}
	}
	*newMsgs = append([]miniagent.Message{summary}, filtered...)
}

// contextKeepRecent 是 compactHistory/compactWithSummary 默认保留的最近轮数。
// P3：从 6 下调到 4——稳态占用降一截，更早的轮次由 summary（已含关键事实）承载；配置钩子
// cfg.Run.ContextKeepRecent 仍在，长 ReAct 场景可调回更高值。
const contextKeepRecent = 4

// §P1-E retainedTail token 预算上下界与分母（移植自 opencode compaction.ts:33-34/80-85 的
// MIN/MAX_PRESERVE_RECENT_TOKENS）。tailBudgetFraction=4 对应 opencode usable*0.25 的 1/4（这里用 ContextWindow/4）。
const (
	minPreserveRecentTokens = 2000
	maxPreserveRecentTokens = 8000
	tailBudgetFraction      = 4
)

// contextKeepReasoning 是主动 reasoning 清理默认保留的最近 assistant 消息条数（P1）。
// 1 = 仅保留最近一条 assistant 的思考链（当前推理上下文），更早的清空。
const contextKeepReasoning = 1

// contextKeepReasoningChars 是保留窗口内单条 Reasoning 的字符上限（P7，rune）。超过则做头尾分段
// （复用 text.TruncateHeadTail 的头 1/4 + 尾 3/4），把思考模型超长 reasoning 的中段发散压掉、两端保留。
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
