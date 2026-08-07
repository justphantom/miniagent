// assemble.go：摘要 prompt、compaction hook 与 NewCompaction 装配入口。

package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

// summaryPrefix 是落盘的 miniagent.KindSummary 消息 Content 的展示层前缀（原硬编码 "[既往对话摘要]\n"）。
// 注意：识别 miniagent.KindSummary 必须用 miniagent.Message.Kind == miniagent.KindSummary（applyCompactionBarrier context.go:164），
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

// stripSummaryPrefix 从 miniagent.Message.Content 剥离 summaryPrefix 还原纯摘要文本供 UPDATE 回灌。
// 主识别必须用 Kind==miniagent.KindSummary；此函数仅在有前缀时剥离，无前缀原样返回（防御旧/手写 session）。
// 注意同级 codepath 有空格变体 "[既往对话摘要] "，此处只剥 production 的 \n 前缀；混合历史下
// 若前缀不匹配直接返回原文，由 UPDATE 指令自行处理（前缀仅展示层，不参与识别）。
func stripSummaryPrefix(content string) string {
	return strings.TrimPrefix(content, summaryPrefix)
}

// summarizeMiddle 调 LLM 把中段 msgs 压成一段摘要文本（不带 tools）。返回经 maxChars 截断的
// 摘要 + 该次调用的 miniagent.Usage（供上游累加入预算）。复用 miniagent.ChatClient.Do；调用方据 error 回落
// 有损压缩（审查 v2 #6）。previousSummary 非空时走 UPDATE 模式（buildSummarizerSystem 判定）。
func summarizeMiddle(ctx context.Context, llm miniagent.Doer, model, summarizerPrompt, previousSummary string, maxChars, maxSummaryTokens int, msgs []miniagent.Message) (string, miniagent.Usage, error) {
	if maxSummaryTokens <= 0 {
		maxSummaryTokens = summaryMaxTokens
	}
	if len(msgs) == 0 {
		return "", miniagent.Usage{}, errors.New("无中段可摘要")
	}
	system := buildSummarizerSystem(summarizerPrompt, previousSummary, maxChars)
	resp, err := llm.Do(ctx, miniagent.Request{
		Model:     model,
		System:    system,
		Messages:  msgs,
		MaxTokens: maxSummaryTokens,
	})
	if err != nil {
		return "", miniagent.Usage{}, err
	}
	return text.Truncate(strings.TrimSpace(resp.Text), maxChars, "…[摘要已截断]"), resp.Usage, nil
}

// CompactingInput 是摘要现场快照，让外部 hook 决定是否注入/替换（§P2，镜像 opencode experimental.session.compacting）。
type CompactingInput struct {
	// SessionID 是本次摘要所属会话 id（经 CompactionOptions.SessionID 注入；cmd 层从 session meta.ID 透传）。空串兼容无 session 模式。
	SessionID string
	// Middle 是待摘要的中段（compactWithSummary 切出的 middle，含已并入的旧 miniagent.KindSummary）。
	// 只读：hook 不应就地改 middle，注入经 CompactingOutput.Context 由 applyCompactingHook 追加。
	Middle []miniagent.Message
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
// context 注入只追加无 tool_calls 的 user 消息，不破坏 tool 配对（调用前已过 miniagent.ValidateToolPairing）。
func applyCompactingHook(ctx context.Context, hook CompactingHook, sessionID, model, summarizerPrompt string, middle []miniagent.Message) (effPrompt string, effMiddle []miniagent.Message, err error) {
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
		injected := append(append([]miniagent.Message{}, middle...), miniagent.Message{Role: miniagent.RoleUser, Content: strings.Join(out.Context, "\n")})
		effMiddle = injected
	}
	return effPrompt, effMiddle, nil
}

// NewCompaction 把整套上下文压缩引擎（FitHistory：超窗摘要中段 + 有损 fallback + 主动裁剪；§P1-B 静默溢出
// 检测）封装为一对可外挂的 LoopHooks 钩子。这是「压缩作为外挂」的默认实现：核心 Run 不含任何压缩，
// 调用方经 NewCompaction(opts) 取回 (before, after) 挂到 LoopHooks.BeforeLLM/AfterLLM 即恢复完整压缩能力；
// 不挂则得极简无压缩 agent。before 每步做 applyCompactionBarrier + FitHistory；after 采真实 usage 判溢出、
// 置下步 Force（跨步共享 overflowPending 状态）。opts.Chat 必须非 nil（摘要 LLM 调用需 client）。
func NewCompaction(opts CompactionOptions) (before func(context.Context, miniagent.StepInput) (miniagent.StepOutput, error), after func(context.Context, int, miniagent.Response) error) {
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
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			if opts.Chat == nil {
				return "", miniagent.Usage{}, errors.New("compaction: Chat 为 nil，无法摘要（须配置 CompactionOptions.Chat）")
			}
			return summarizeMiddle(ctx, opts.Chat, model, sys, prevSummary, maxChars, opts.SummaryMaxTokens, middle)
		},
	}
	before = func(ctx context.Context, in miniagent.StepInput) (miniagent.StepOutput, error) {
		barrier := applyCompactionBarrier(in.Msgs)
		// §P1-B：从上一步已入史的 assistant.Usage 判静默溢出（撞 provider 前先压）。每步从 in.Msgs
		// 推断 Force 到局部变量，再拷贝 budget 传入 FitHistory——闭包不写共享状态，多 Run 并发复用
		// 同一钩子亦无 race（ContextBudget 是值类型，b 与 budget 共享只读的 Summarize/Tools 等引用）。
		force := false
		if idx := lastApplicableUsageIndex(in.Msgs); idx >= 0 {
			force = isUsageOverflow(*in.Msgs[idx].Usage, opts.ContextWindow, opts.MaxTokens, opts.Reserved, opts.Auto)
		}
		b := budget
		b.Force = force
		fitted, summary, summarized, sumUsage, err := FitHistory(ctx, barrier, b, opts.Logger)
		if err != nil {
			return miniagent.StepOutput{}, err
		}
		out := miniagent.StepOutput{View: fitted, Commit: true} // 压缩：收缩后的 fitted 即新 transcript
		if summarized {
			out.Persist = []miniagent.Message{summary}
			u := sumUsage
			out.ExtraUsage = &u
			out.Compacted = true
		}
		return out, nil
	}
	// after 不再持有跨步状态：溢出检测移到 before 从 in.Msgs 推断。返回 nil 让调用方跳过无意义的 AfterLLM。
	after = nil
	return before, after
}
