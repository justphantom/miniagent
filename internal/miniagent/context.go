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

// summaryPrefix 是落盘的 KindSummary 消息 Content 的展示层前缀（原硬编码 "[既往对话摘要]\n"）。
// 注意：识别 KindSummary 必须用 Message.Kind == KindSummary（applyCompactionBarrier context.go:164），
// 不可用前缀字符串嗅探——历史上有空格变体 "[既往对话摘要] "（非落盘路径）与测试 fixture 不一致。
// 前缀仅展示层，不参与识别。
const summaryPrefix = "[既往对话摘要]\n"

// summaryCreateInstruction 是 CREATE 模式角色指令；%d 由 fmt.Sprintf 注入 maxChars，
// 与旧 summarizerSystem 的 %d 契约一致（用户 override 仍按此契约）。
const summaryCreateInstruction = "你是会话压缩器。把以下对话历史压缩为不超过 %d 字符的锚定摘要，严格按下方模板结构输出。"

// summaryUpdateInstruction 是 UPDATE 模式角色指令（移植 opencode buildPrompt compaction.ts:164
// 的 preserve/remove/merge）。后面由 buildSummarizerSystem 追加 <previous-summary> 块 + 模板。
const summaryUpdateInstruction = "你是会话压缩器。基于以下对话历史更新已有的锚定摘要，输出不超过 %d 字符。保留仍成立的事实，删除已过时的细节，合并新增的事实。把旧摘要作为锚点更新："

// summaryTemplate 是固定 6 段 Markdown 模板（移植 opencode SUMMARY_TEMPLATE compaction.ts(core):16-46，
// 中文化匹配现有 prompt 风格）。CREATE/UPDATE 两模式都追加在指令之后。
const summaryTemplate = `严格按以下 Markdown 结构输出，保持段落顺序，不要输出 <template> 标签。
<template>
## 目标
- [用户想完成什么，一两句简述]

## 关键细节
- [约束/偏好、决策及理由、重要事实/假设、续跑所需确切上下文，或 "(无)"]

## 进展状态
### 已完成
- [已完成的工作、已验证的事实、已做的改动，或 "(无)"]

### 进行中
- [当前工作、未完成的改动、调查中的状态，或 "(无)"]

### 受阻
- [阻碍、失败的命令、未知项，或 "(无)"]

## 下一步
1. [立即要做的具体动作，或 "(无)"]
2. [若已知，下一个动作，或 "(无)"]

## 相关文件
- [文件或目录路径：为什么重要，或 "(无)"]
</template>

规则：
- 保留每个段落，即使为空也输出 "(无)"。
- 用简短要点，不要写成段落。
- 已知的文件路径、符号、命令、错误串、URL、标识符必须原样保留。
- 不要提及摘要/压缩过程本身。`

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
	// UseRealUsage 控制 estimateThreshold 是否优先采纳真实 usage（§P0-B）。false（kill-switch）
	// 直接用本地 estimateTokens，回落旧行为；true 时无可用真实 usage 亦自动回落本地估算。
	UseRealUsage bool
	// Force 为 true 时 FitHistory 跳过 estimateTokens 4/5 门控，直接进入 compactWithSummary+有损 fallback
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
	// Summarize 把中段 middle 压成摘要文本（已截断到 maxChars）+ 该次调用 Usage。
	// previousSummary 非空时走 UPDATE 模式（保留旧摘要作锚点更新），由 compactWithSummary
	// 从遗留 KindSummary 抽出经此参数下传。Run 注入：func(ctx, model, summarizerPrompt,
	// previousSummary, middle) → summarizeMiddle(...)。
	Summarize func(ctx context.Context, model, summarizerPrompt, previousSummary string, middle []Message) (string, Usage, error)
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
	// §P0-B：阈值判定改用 estimateThreshold（优先真实 usage、回落本地估算），补 estimateTokens
	// 对缓存内容零感知的盲区，长会话不再系统性偏晚触发压缩。
	// §P1-B：Force=true（上一步真实 usage 命中 isUsageOverflow）时跳过估算门控，直接进 compactWithSummary
	// 分支，让压缩基于「已证实的真实占用」触发。Force 仅在 ContextWindow>0 时被置位，故不产生 ContextWindow<=0
	// 的非法 Force 路径；后续 trimRecentRounds/终止门控（:116/:122）仍用 ContextWindow，不受影响。
	if !budget.Force && (budget.ContextWindow <= 0 || estimateThreshold(msgs, budget.System, budget.Tools, budget.UseRealUsage) <= budget.ContextWindow*4/5) {
		// P1/P4/P6/P7/P8'/P9b/P11 主动裁剪（各阶段语义见 applyContextStrips；Debug level 记录节省 token）。
		out := applyContextStrips(ctx, msgs, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
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
	// P1/P4/P6/P7/P8'/P9b/P11 主动裁剪（语义见 applyContextStrips；放在 window 检查前——清理后 token 估计更低，
	// 更可能免于触发 trimRecentRounds/终止报错。中段已并入 summary 不经此处）。
	out = applyContextStrips(ctx, out, keepReasoning, keepReasoningChars, keepToolArgs, logger, budget.System, budget.Tools)
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

// applyContextStrips 跑全部主动裁剪（P1/P4/P6/P7/P8'/P9b/P11），仅改 context 侧拷贝，供 FitHistory
// 未超窗/超窗两分支复用（原两处内联序列一致，抽此避免重复 + 统一可观测）。logger 为 Debug level
// （CLI -log-level debug）时，记录各阶段节省的 token（estimateTokens 差值）与 fit 前后总量，供 v11 §6
// 的「确实省了」运行时确认；Info level（默认）不算差值、零开销。各阶段语义见各 strip 函数 doc。
func applyContextStrips(ctx context.Context, msgs []Message, keepReasoning, keepReasoningChars, keepToolArgs int, logger *slog.Logger, sys string, tools []Tool) []Message {
	dbg := logger != nil && logger.Enabled(ctx, slog.LevelDebug)
	strip := func(stage string, fn func([]Message) []Message, in []Message) []Message {
		if !dbg {
			return fn(in)
		}
		before := estimateTokens(in, sys, tools)
		o := fn(in)
		if after := estimateTokens(o, sys, tools); before > after {
			logger.Debug("context budget: strip saved",
				"stage", stage, "saved_tokens", before-after, "before_msgs", len(in), "after_msgs", len(o))
		}
		return o
	}
	out := strip("P1_reasoning", func(m []Message) []Message { return stripStaleReasoning(m, keepReasoning) }, msgs)
	out = strip("P7_reasoningTrunc", func(m []Message) []Message { return truncateKeptReasoning(m, keepReasoning, keepReasoningChars) }, out)
	out = strip("P4_toolArgs", func(m []Message) []Message { return stripStaleToolArgs(m, keepToolArgs) }, out)
	out = strip("P6_dedupRead", func(m []Message) []Message { return dedupReadResults(m, keepToolArgs) }, out)
	out = strip("P11_foldRead", func(m []Message) []Message { return foldStaleReadResults(m, keepToolArgs) }, out)
	out = strip("P8p_foldWriteEdit", func(m []Message) []Message { return foldStaleWriteEditArgs(m, keepToolArgs) }, out)
	out = strip("P9b_dedupShell", func(m []Message) []Message { return dedupShellCommands(m, keepToolArgs) }, out)
	if dbg {
		logger.Debug("context budget: fit done",
			"before_tokens", estimateTokens(msgs, sys, tools), "after_tokens", estimateTokens(out, sys, tools), "msgs", len(out))
	}
	return out
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

// buildSummarizerSystem 集中构造摘要 system prompt（替代旧内联 system 选择）。规则：
//   - summarizerPrompt 非空 → 全量 override（fmt.Sprintf(summarizerPrompt, maxChars)），
//     向后兼容用户自定义，忽略 previousSummary（override 路径维持旧的「旧摘要并入 middle」行为）。
//   - 默认路径 + previousSummary 非空 → UPDATE（summaryUpdateInstruction + <previous-summary>
//     块包裹旧摘要 + summaryTemplate）：旧摘要不再作为 history 重读重写，省一半 token、显式
//     preserve 指令降低丢细节概率。
//   - 默认路径 + previousSummary 为空 → CREATE（summaryCreateInstruction + summaryTemplate）。
func buildSummarizerSystem(summarizerPrompt, previousSummary string, maxChars int) string {
	if summarizerPrompt != "" {
		return fmt.Sprintf(summarizerPrompt, maxChars)
	}
	if previousSummary != "" {
		return fmt.Sprintf(summaryUpdateInstruction, maxChars) +
			"\n<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n" +
			summaryTemplate
	}
	return fmt.Sprintf(summaryCreateInstruction, maxChars) + "\n\n" + summaryTemplate
}

// stripSummaryPrefix 从 Message.Content 剥离 summaryPrefix 还原纯摘要文本供 UPDATE 回灌。
// 主识别必须用 Kind==KindSummary；此函数仅在有前缀时剥离，无前缀原样返回（防御旧/手写 session）。
// 注意同级 codepath 有空格变体 "[既往对话摘要] "，此处只剥 production 的 \n 前缀；混合历史下
// 若前缀不匹配直接返回原文，由 UPDATE 指令自行处理（前缀仅展示层，不参与识别）。
func stripSummaryPrefix(content string) string {
	return strings.TrimPrefix(content, summaryPrefix)
}

// summarizeMiddle 调 LLM 把中段 msgs 压成一段摘要文本（不带 tools）。返回经 maxChars 截断的
// 摘要 + 该次调用的 Usage（供上游累加入预算）。复用 ChatClient.Do；调用方据 error 回落
// 有损压缩（审查 v2 #6）。previousSummary 非空时走 UPDATE 模式（buildSummarizerSystem 判定）。
func summarizeMiddle(ctx context.Context, llm *ChatClient, model, summarizerPrompt, previousSummary string, maxChars int, msgs []Message) (string, Usage, error) {
	if len(msgs) == 0 {
		return "", Usage{}, errors.New("无中段可摘要")
	}
	system := buildSummarizerSystem(summarizerPrompt, previousSummary, maxChars)
	resp, err := llm.Do(ctx, Request{
		Model:     model,
		System:    system,
		Messages:  msgs,
		MaxTokens: getSummaryMaxTokens(),
	})
	if err != nil {
		return "", Usage{}, err
	}
	return truncate(strings.TrimSpace(resp.Text), maxChars, "…[摘要已截断]"), resp.Usage, nil
}

// CompactingInput 是摘要现场快照，让外部 hook 决定是否注入/替换（§P2，镜像 opencode experimental.session.compacting）。
type CompactingInput struct {
	// SessionID 是本次摘要所属会话 id（Run 经 LoopConfig.SessionID 透传）。空串兼容无 session 模式。
	SessionID string
	// Middle 是待摘要的中段（compactWithSummary 切出的 middle，含已并入的旧 KindSummary）。
	// 只读：hook 不应就地改 middle，注入经 CompactingOutput.Context 由 applyCompactingHook 追加。
	Middle []Message
	// Model 是实际用于本次摘要的模型 id（已回落 CompactionModel→Model）。
	Model string
}

// CompactingOutput 是 hook 的回参：注入 context 或一次性替换 summarizerPrompt。
type CompactingOutput struct {
	// Context 追加到摘要输入的额外文本（领域知识/文件清单/外部记忆等）；空切片=不注入。
	// applyCompactingHook 以一条 role=user 消息 append 到 middle 末尾，进摘要输入而非 system
	// （对齐 opencode compaction.ts nextPrompt 进 user 通道的语义）。
	Context []string
	// Prompt 非空时替换本次 summarizerPrompt（仅本次调用，不持久改 budget.SummarizerPrompt）。
	// 空串=沿用 budget.SummarizerPrompt。
	Prompt string
}

// CompactingHook 在每次摘要前同步触发（compactWithSummary 内、调 budget.Summarize 前）。
// 镜像 opencode experimental.session.compacting：可注入 context 或替换 prompt，不可 cancel
// （pi 才支持 cancel；miniagent 现阶段不支持）。
// 契约（实现 A，与 opencode plugin.trigger 默认语义一致）：hook 抛错上抛中止压缩。
// nil = 无 hook，applyCompactingHook 零开销短路。
type CompactingHook func(ctx context.Context, in CompactingInput) (CompactingOutput, error)

// applyCompactingHook 在 compactWithSummary 内、调 budget.Summarize 之前触发 hook（§P2），
// 把 hook 输出折叠回 (effPrompt, effMiddle)：Prompt 非空覆盖本次 summarizerPrompt；Context 非空
// 以一条 role=user 消息 append 到 middle 末尾（进摘要输入，不进 system 通道）。
// 契约：hook==nil 直接返回原 prompt/middle（零开销短路）。hook 返回 error → 上抛中止压缩（实现 A）。
// context 注入只追加无 tool_calls 的 user 消息，不破坏 tool 配对（调用前已过 validateToolPairing）。
func applyCompactingHook(ctx context.Context, hook CompactingHook, sessionID, model, summarizerPrompt string, middle []Message) (effPrompt string, effMiddle []Message, err error) {
	if hook == nil {
		return summarizerPrompt, middle, nil
	}
	out, err := hook(ctx, CompactingInput{SessionID: sessionID, Middle: middle, Model: model})
	if err != nil {
		return "", nil, err
	}
	effPrompt = summarizerPrompt
	if out.Prompt != "" {
		effPrompt = out.Prompt
	}
	effMiddle = middle
	if len(out.Context) > 0 {
		injected := append(append([]Message{}, middle...), Message{Role: roleUser, Content: strings.Join(out.Context, "\n")})
		effMiddle = injected
	}
	return effPrompt, effMiddle, nil
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
func estimateRoundTokens(round []Message) int {
	return estimateTokens(round, "", nil) - systemOverheadTokens
}

// selectTailByTokens 按 token 预算从最近轮累加选 tail（移植 opencode select，§P1-E）。
// maxTurns=keepRecent（轮数上界）；tokenBudget=preserveRecentTokens(...)。流程：从最近轮向前累加
// estimateRoundTokens(轮)（边际，不含 system/schema），整轮装下并入 tail；首个装不下的边界轮调
// splitRoundByTokens 找安全切点（切出后缀并入 tail），切不动（tool-call 轮）转 shrinkRoundToolContents
// 压 tool content 贴合剩余预算并入 tail，仍不行则整轮进 middle；边界轮之前的全部进 middle。
// tokenBudget<=0 退化为「最近 maxTurns 轮」纯轮数模式（向后兼容）。返回 tail 与 middle 均为扁平 []Message（原顺序）。
func selectTailByTokens(rounds [][]Message, maxTurns, tokenBudget int) (tail, middle []Message) {
	n := len(rounds)
	if n == 0 {
		return nil, nil
	}
	if tokenBudget <= 0 {
		cnt := min(maxTurns, n)
		return flatten(rounds[n-cnt:]), flatten(rounds[:n-cnt])
	}
	total := 0
	// tailStart 初值 0：若全部轮都装下（n<=maxTurns 且未触 token 上界，循环正常结束）则 tail=全部、middle=空。
	// 循环内 break 时覆写为 i+1（tail=rounds[i+1..]）。
	tailStart := 0
	boundary := -1 // token 边界轮索引（需 split/shrink 决策）；-1=无（全部装下或仅 maxTurns 截断）
	for i := n - 1; i >= 0; i-- {
		if n-i > maxTurns {
			tailStart = i + 1 // maxTurns 截断：tail=rounds[i+1..]，older 进 middle
			break
		}
		size := estimateRoundTokens(rounds[i])
		if total+size > tokenBudget {
			boundary = i
			tailStart = i + 1
			break
		}
		total += size
	}
	tail = flatten(rounds[tailStart:n])
	middle = flatten(rounds[:tailStart])
	if boundary >= 0 {
		// 边界轮尝试 split/shrink 并入 tail 前端；成功则该轮（压缩后）不进 middle。
		remaining := tokenBudget - total
		if fitted, ok := splitOrShrinkToRound(rounds[boundary], remaining); ok {
			tail = append(append([]Message{}, fitted...), tail...)
			middle = flatten(rounds[:boundary]) // boundary 轮已并入 tail，从 middle 移除
		}
		// split/shrink 失败 → 边界轮整轮留 middle（rounds[:tailStart]=rounds[:boundary+1] 已含），符合预期。
	}
	return tail, middle
}

// splitOrShrinkToRound 是边界轮的适配入口：先试 splitRoundByTokens 找安全消息边界切点（后缀并入 tail），
// 切不动则 shrinkRoundToolContents 压 tool content 贴合 remaining。返回 (fitted, ok)；ok=false 表示
// 边界轮无法并入 tail，应整轮进 middle。
func splitOrShrinkToRound(round []Message, remaining int) ([]Message, bool) {
	if remaining <= 0 {
		return nil, false
	}
	if suffix := splitRoundByTokens(round, remaining); suffix != nil {
		return suffix, true
	}
	shrunk := shrinkRoundToolContents(round, remaining)
	if estimateRoundTokens(shrunk) <= remaining {
		return shrunk, true
	}
	return nil, false
}

// splitRoundByTokens 单轮内按 token 预算找安全切点，返回并入 tail 的后缀（移植 opencode splitTurn，§P1-E）。
// 受 miniagent flat []Message 配对约束：tool 角色消息不可作切点起点（会孤立 tool 于 assistant 之外），
// 后缀须自洽（validateToolPairing 守）。从 round[1] 起向后扫，返回最早 estimateRoundTokens<=tokenBudget 的合格后缀。
// tokenBudget<=0 或 len(round)<=1（单消息轮无可切边界）返回 nil——由调用方整轮进 middle 或转 shrink。
// 注：miniagent splitRounds 使文本轮为单消息、tool-call 轮为 [assistant(tc)+tools]，故本函数对生产轮恒返回 nil
// （单消息轮 len<=1；tool-call 轮除 round[0] 外皆 tool 被跳过）；保留以服务手动构造的多消息轮与未来扩展。
func splitRoundByTokens(round []Message, tokenBudget int) []Message {
	if tokenBudget <= 0 || len(round) <= 1 {
		return nil
	}
	for i := 1; i < len(round); i++ {
		if round[i].Role == roleTool {
			continue // 切点后缀不能以 tool 开头（孤立）
		}
		suffix := round[i:]
		if estimateRoundTokens(suffix) <= tokenBudget {
			if err := validateToolPairing(suffix); err != nil {
				continue
			}
			return suffix
		}
	}
	return nil
}

// shrinkRoundToolContents 是 miniagent flat 模型下 opencode splitTurn 的语义等价物（§P1-E，REFUTED 后的必需补偿）：
// tool-call 轮切不动时，把轮内 tool 结果 content 就地截短贴合 tokenBudget（深拷贝，不动入参，保配对不变）。
// 按 round 当前 estimateRoundTokens 与 tokenBudget 的比例缩放每条 tool content 字符数（复用 truncateHeadTail 头1/4+尾3/4）。
// tokenBudget<=0 原样返回拷贝；无 tool 消息则压缩无意义但仍返回拷贝（由调用方判 fit）。
func shrinkRoundToolContents(round []Message, tokenBudget int) []Message {
	out := make([]Message, len(round))
	copy(out, round)
	if tokenBudget <= 0 {
		return out
	}
	cur := estimateRoundTokens(out)
	if cur <= tokenBudget {
		return out
	}
	ratio := float64(tokenBudget) / float64(cur)
	if ratio > 1 {
		ratio = 1
	}
	for i, m := range out {
		if m.Role == roleTool && len(m.Content) > 0 {
			newLen := int(float64(len([]rune(m.Content))) * ratio)
			newLen = max(1, newLen)
			out[i].Content = truncateHeadTail(m.Content, newLen, "…[tool 结果已压缩]")
		}
	}
	return out
}

// compactWithSummary 保留最早 1 轮 + 最近 keepRecent 轮，中段摘要为单条 KindSummary 消息。
// 返回 (out, summary, usage, err)：out 含新 summary；summary 是该消息（.Kind=="" 表示无中段/失败）；
// 中段配对断裂或摘要失败返回 error（调用方 FitHistory 回落 compactHistory）。无中段可摘返回
// (msgs, Message{}, Usage{}, nil)。不再接收 newMsgs——持久化插入由 Run 完成。
//
// 跨轮继承（P2-1 + §P0-A UPDATE）：上轮 LoadSession 带入的旧 summary 经 applyCompactionBarrier
// 落在 msgs 头，splitRounds 使其单独成 rounds[0]。
//   - 默认路径（SummarizerPrompt==""）：用 stripSummaryPrefix 抽出旧摘要文本作 previousSummary
//     经 Summarize 回调下传（UPDATE 模式），head 置 nil、旧摘要不再并入 middle——省一半 token
//     （旧摘要不再作为 history 重读重写）、显式 preserve 指令降低丢细节概率。这是 99% 用户路径。
//   - override 路径（SummarizerPrompt!=""）：维持旧行为，旧 summary 并入 middle 开头让 LLM 重读
//     重写（previousSummary 传空），已设自定义 prompt 的用户零回归。
//
// 下轮 barrier 命中新 summary 后旧 summary 被丢弃；首轮非 summary（正常 user 轮）维持原行为。
func compactWithSummary(ctx context.Context, budget ContextBudget, msgs []Message, keepRecent int) (out []Message, summary Message, usage Usage, err error) {
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs, Message{}, Usage{}, nil // 无中段可摘
	}
	// §P1-E：tail 选择从纯轮数升级为 token 预算累加 + 边界轮细切（tokenBudget<=0 回落纯轮数，向后兼容）。
	// 在 rounds[1:]（排除 head=rounds[0]）上选 tail，head 单独保留/并入 middle。
	tokenBudget := preserveRecentTokens(budget)
	tail, middleCore := selectTailByTokens(rounds[1:], keepRecent, tokenBudget)
	head := rounds[0]
	prevSummary := ""
	if len(head) == 1 && head[0].Kind == KindSummary {
		if budget.SummarizerPrompt == "" {
			// 默认路径：抽旧摘要作 UPDATE 锚点，不再并入 middle 重读重写。
			prevSummary = stripSummaryPrefix(head[0].Content)
		} else {
			// override 路径：维持旧行为，旧 summary 并入 middle 让 LLM 重读重写。
			middleCore = append([]Message{head[0]}, middleCore...)
		}
		head = nil
	}
	middle := middleCore
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
	// §P2：摘要 LLM 调用前触发 compaction hook（注入 context / 一次性替换 summarizerPrompt）。
	// 必须排在 validateToolPairing(middle) 通过之后：context 注入仅追加无 tool_calls 的 user 消息，不破坏配对。
	effPrompt, effMiddle, herr := applyCompactingHook(ctx, budget.Compacting, budget.SessionID, compModel, budget.SummarizerPrompt, middle)
	if herr != nil {
		return msgs, Message{}, Usage{}, herr // 实现A：hook 抛错上抛中止压缩
	}
	text, sumUsage, err := budget.Summarize(ctx, compModel, effPrompt, prevSummary, effMiddle)
	if err != nil {
		return msgs, Message{}, Usage{}, err
	}
	// §P0-B：summaryMsg 显式打新戳 nowMs()（不经 appendMsg）——防陈旧的关键触发点：新 Ts 使其前
	// assistant 的真实 usage 在下一轮 estimateTokensFromUsage 中失效（lastApplicableUsageIndex 的
	// latestSummaryTs 抬高），强制回落本地估算重算压缩后的小体积历史，避免陈旧大 usage 立即二次压缩。
	summaryMsg := Message{Role: roleUser, Kind: KindSummary, Content: summaryPrefix + text, Ts: nowMs()}
	out = append([]Message{}, head...)
	out = append(out, summaryMsg)
	out = append(out, tail...)
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

// NewCompaction 把整套上下文压缩引擎（FitHistory：超窗摘要中段 + 有损 fallback + 主动裁剪；§P1-B 静默溢出
// 检测）封装为一对可外挂的 LoopHooks 钩子。这是「压缩作为外挂」的默认实现：核心 Run 不含任何压缩，
// 调用方经 NewCompaction(opts) 取回 (before, after) 挂到 LoopHooks.BeforeLLM/AfterLLM 即恢复完整压缩能力；
// 不挂则得极简无压缩 agent。before 每步做 applyCompactionBarrier + FitHistory；after 采真实 usage 判溢出、
// 置下步 Force（跨步共享 overflowPending 状态）。opts.Chat 必须非 nil（摘要 LLM 调用需 client）。
func NewCompaction(opts CompactionOptions) (before func(context.Context, StepInput) (StepOutput, error), after func(context.Context, int, Response) error) {
	maxChars := opts.SummaryMaxChars
	if maxChars <= 0 {
		maxChars = summaryMaxChars
	}
	budget := ContextBudget{
		ContextWindow:        opts.ContextWindow,
		KeepRecent:           opts.KeepRecent,
		KeepReasoning:        opts.KeepReasoning,
		KeepToolArgs:         opts.KeepToolArgs,
		KeepReasoningChars:   opts.KeepReasoningChars,
		SummarizerPrompt:     opts.SummarizerPrompt,
		CompactionModel:      opts.CompactionModel,
		Model:                opts.Model,
		System:               opts.System,
		Tools:                opts.Tools,
		UseRealUsage:         opts.UseRealUsage,
		PreserveRecentTokens: opts.PreserveRecentTokens,
		Compacting:           opts.OnCompacting,
		SessionID:            opts.SessionID,
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []Message) (string, Usage, error) {
			return summarizeMiddle(ctx, opts.Chat, model, sys, prevSummary, maxChars, middle)
		},
	}
	var overflowPending bool
	before = func(ctx context.Context, in StepInput) (StepOutput, error) {
		barrier := applyCompactionBarrier(in.Msgs)
		budget.Force = overflowPending
		fitted, summary, summarized, sumUsage, err := FitHistory(ctx, barrier, budget, opts.Logger)
		if err != nil {
			return StepOutput{}, err
		}
		out := StepOutput{View: fitted, Commit: true} // 压缩：收缩后的 fitted 即新 transcript
		if summarized {
			out.Persist = []Message{summary}
			u := sumUsage
			out.ExtraUsage = &u
			out.Compacted = true
		}
		return out, nil
	}
	after = func(_ context.Context, _ int, resp Response) error {
		// §P1-B：用上一步真实 usage 判静默溢出，置下步 Force（撞 provider 前先压）。
		overflowPending = isUsageOverflow(resp.Usage, opts.ContextWindow, opts.MaxTokens, opts.Reserved, opts.Auto)
		return nil
	}
	return before, after
}
