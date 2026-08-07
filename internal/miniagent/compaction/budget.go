// budget.go：预算估算、配置常量与 FitHistory 入口。

package compaction

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
)

const (
	// summaryMaxChars 是摘要字符上限的内置上界（初值，按实测调，设计 §15.4）。默认经 deriveSummaryMaxChars
	// 按 min(summaryMaxChars, ContextWindow/summaryCharsPerWindowRatio) 随窗口缩放（方向 A）——大窗口取此值，
	// 小窗口自适应，避免 summary 本身 > CW×4/5 致压缩后终止（B 的边界）。用户显式 summary_max_chars 覆盖。
	summaryMaxChars = 5000
	// summaryCharsPerWindowRatio：默认 summaryMaxChars = ContextWindow/此值。取 5 → summary token（chars/2）
	// 占 CW ~10%，给 head+tail+LLM 输出留 ~90%——平衡「摘要信息量」与「不挤压 tail/输出」。更小值（如 8）摘要
	// 更精简、CW 下界更低但信息量降；更大值（如 4）反之。对标 tailBudgetFraction 内置比例风格；用户显式
	// summary_max_chars 覆盖派生。注：即便 summary 缩到 0，CW<~1.5k 仍可能终止（请求级 overhead 400 + system
	// + schema + head 占 CW 过半，物理极限，非本比例可解，详见 deriveSummaryMaxChars 硬边界）。
	summaryCharsPerWindowRatio = 5
	// summaryMaxTokens 是摘要输出 token 上限的兜底常量，从 summaryMaxChars 派生（/2）。
	// 口径与 EstimateTokens 的 CJK≈1token/2chars 同源：chars/2 恰是「纯 CJK 摘要填满 chars 上限」
	// 所需的 token 上界——纯中文刚好填满（边界），任何更稀疏内容（英文路径/符号）由 chars 先截，
	// token 不额外截断也不浪费。原固定 1024 对 CJK 偏紧：1024 token≈1500 汉字，远低于
	// summaryMaxChars=5000，中文摘要被 MaxTokens 隐性截短到设计值约 30%（third-evaluation L3-5）。
	// 既是 summarizeMiddle(maxSummaryTokens<=0) 的兜底，也由 deriveSummaryMaxTokens 在异常输入时回落。
	summaryMaxTokens = summaryMaxChars / 2
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
	// SummaryMaxChars 是摘要字符上限，供 jointTailBudget 估算摘要 token 占用（CJK /2 口径）。<=0 回落
	// summaryMaxChars 常量。由 NewCompaction 从 opts.SummaryMaxChars（已解析）注入。
	SummaryMaxChars int
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
//   - committed：本轮 out 是否应替换运行 transcript（压缩成功/fallback=true；非压缩 strip 仅本轮 View=false）。
//     非压缩不替换 → transcript 保留原文（reasoning/args 不被滚动 strip 丢失）；压缩替换为 [head,summary,tail]，
//     tail 保留原文（不再 strip out），RewriteMessages 落盘完整近期上下文（消除压缩轮持久化不对称）。
//   - usage：摘要调用的 token 用量（Run 累加进 total，MaxTotalTokens 预算含摘要调用）；
//   - err：即使有损裁剪后仍超 window 时返回，Run 应终止避免循环烧请求。
//
// 不触碰 newMsgs——持久化层的 summary 插入/去重由 Run 经 mergePersisted 完成（loop.go:216）。
func FitHistory(ctx context.Context, msgs []miniagent.Message, budget ContextBudget, logger *slog.Logger) (out []miniagent.Message, summary miniagent.Message, summarized, committed bool, usage miniagent.Usage, err error) {
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
	// 门控基于 strip View（=LLM 所见），非原文 msgs：非压缩步 Commit=false 后 transcript 保留原文，每轮从原文重算 strip。
	// §P0-B：estimateThreshold 优先真实 usage、回落本地估算，补 miniagent.EstimateTokens 对缓存内容零感知的盲区。
	// §P1-B：Force=true（上一步真实 usage 命中 isUsageOverflow）时跳过门控，直接进压缩分支（Force 仅 CW>0 置位）。
	if !budget.Force {
		stripped := applyContextStrips(ctx, msgs, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
		if budget.ContextWindow <= 0 || estimateThreshold(stripped, budget.System, budget.Tools, budget.UseRealUsage) <= budget.ContextWindow*4/5 {
			// 非压缩：strip 仅本轮 View（committed=false），transcript 保留原文，下轮从原文重算 strip。
			return stripped, miniagent.Message{}, false, false, miniagent.Usage{}, nil
		}
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
		// fallback 有损裁剪（原文）：无 jointTailBudget 兜底，保留 strip 防超窗。
		out = compactHistory(msgs, keepRecent)
		out = applyContextStrips(ctx, out, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
	}
	// summarized：out=fitted 不 strip——tail 原文 reasoning/args 保留，体积由 jointTailBudget 控制（≤ CW×4/5）。
	// 压缩后判定用本地 EstimateTokens（门控用 estimateThreshold 反映压缩前前缀；压缩后真实 usage 陈旧，本地更准）。
	if miniagent.EstimateTokens(out, budget.System, budget.Tools) > budget.ContextWindow*4/5 {
		out = trimRecentRounds(out, keepRecent)
		if logger != nil {
			logger.Warn("仍超 window，裁到最近轮", "msgs", len(out))
		}
	}
	// 缓存 post-trim out 的 token 估算：下方判定与错误消息复用同一值（原三次 EstimateTokens 之一在此消除）。
	est := miniagent.EstimateTokens(out, budget.System, budget.Tools)
	if est > budget.ContextWindow*4/5 {
		return out, sm, summarized, true, sumUsage, fmt.Errorf("history 超 context window（约 %d tokens）即使有损裁剪后仍超——终止以避免循环烧请求", est)
	}
	return out, sm, summarized, true, sumUsage, nil
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

// jointTailBudget 返回 retainedTail 的联合 token 预算（§B）：从 CW×4/5 扣除不可压缩部分——请求级
// system/schema/overhead（EstimateTokens([],System,Tools)，整请求出现一次）+ head 轮边际 + 摘要上限估算
// （summaryMaxChars/2，CJK 最密口径，与 summaryMaxTokens 派生同源 + 单条信封）——使 tail 主动让出空间给
// 不可压缩的 summary，从源头减少中等 CW 下 head+summary+tail 超窗致 trim/终止。与 preserveRecentTokens
// （用户 tail 意愿上界）取 min：物理约束 ∧ 意愿上界。CW<=0 回落 preserveRecentTokens（无窗口纯轮数兼容）。
//
// headAdj 精确化：默认路径（SummarizerPrompt==""）下 head 是旧 KindSummary 时，它被抽为 prevSummary
// 走 UPDATE、不进 out，故 headAdj=0 不误扣；其余（首轮非 summary、override 路径）head 进 out → 扣
// estimateRoundTokens(head)。avail<=0（小 CW：summary+head+overhead 已占满 4/5）→ 返回 0，selectTailByTokens
// 退化最近轮强制保留、FitHistory 末尾 trim/error 兜底。本函数不消除极小 CW 终止；方向 A
// （deriveSummaryMaxChars 随 CW 缩放 summaryMaxChars）把该边界上推至 CW<~1536——此后请求级 overhead+head
// 占主导，非压缩预算可解（物理极限，详见 deriveSummaryMaxChars 硬边界）。
func jointTailBudget(budget ContextBudget, headRounds []miniagent.Message) int {
	if budget.ContextWindow <= 0 {
		return preserveRecentTokens(budget)
	}
	target := budget.ContextWindow * 4 / 5
	reqOverhead := miniagent.EstimateTokens(nil, budget.System, budget.Tools)
	headAdj := 0
	if len(headRounds) != 1 || headRounds[0].Kind != miniagent.KindSummary || budget.SummarizerPrompt != "" {
		headAdj = estimateRoundTokens(headRounds)
	}
	maxChars := budget.SummaryMaxChars
	if maxChars <= 0 {
		maxChars = summaryMaxChars
	}
	summaryEstimate := maxChars/2 + miniagent.EnvelopePerMsgTokens
	avail := target - reqOverhead - headAdj - summaryEstimate
	return min(max(avail, 0), preserveRecentTokens(budget))
}

// estimateRoundTokens 估算单轮的边际 token（content+reasoning+args+信封），不计 system/schema 全局开销
// ——这两项是请求级常量，在 tail 总量里只计一次，故逐轮累加时必须用边际估算（否则每轮重复计 400+schema，
// 系统性高估、tail 恒不达标）。供 selectTailByTokens 累加。
func estimateRoundTokens(round []miniagent.Message) int {
	return miniagent.EstimateTokens(round, "", nil) - miniagent.SystemOverheadTokens
}

// insertSummaryIntoNewMsgs 剔除 newMsgs 中已有的 miniagent.KindSummary（单轮多次压缩去重），再把 summary
// 前插使其排在 user_prompt 之前（跨轮 barrier 命中最新 summary）。生产路径用更通用的 mergePersisted
// （loop.go，处理任意带 Kind 的持久化条目）；此函数是单 KindSummary 特化，留供 compaction 端到端测试构造 newMsgs。
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
