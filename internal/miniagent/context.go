package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode"
)

const (
	// summaryMaxChars 是摘要的字符上限（初值，按实测调，设计 §15.4）。
	summaryMaxChars = 2000
	// summaryMaxTokens 限制摘要请求的输出 token 数：端点默认上限远超 summaryMaxChars
	// 对应的 token 量，不限会先生成超长输出再被字符截断，白烧 token（审查 P3-1）。
	summaryMaxTokens = 1024
)

// summarizerSystem 是压缩专用 system prompt：要求把历史压成一段受限摘要，保留关键事实。
// 默认值通过 LoopConfig.SummarizerPrompt 覆盖；调用方需传入 maxChars（summaryMaxChars）。
const summarizerSystem = "你是会话压缩器。把以下对话历史压缩为一段不超过 %d 字符的中文摘要，保留关键事实、决策、文件改动与未决问题，不要复述全部对话，不要输出多余解释。"

// ContextBudget 是 FitHistory 的单一参数包：上下文窗口、保留轮数、摘要提示与模型、
// 以及把中段压成摘要的可注入回调（解耦 context.go 与 HTTPClient，便于测试注入假摘要）。
// System/Tools 用于 estimateTokens 估算（计入 system prompt 与工具 schema 的固定开销）。
type ContextBudget struct {
	ContextWindow    int // 0 = 不主动压缩
	KeepRecent       int // <=0 回落 contextKeepRecent
	SummarizerPrompt string
	CompactionModel  string // 空则回落 Model
	Model            string
	System           string
	Tools            []Tool
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
	if budget.ContextWindow <= 0 || estimateTokens(msgs, budget.System, budget.Tools) <= budget.ContextWindow*4/5 {
		return msgs, Message{}, false, Usage{}, nil
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
		MaxTokens: summaryMaxTokens,
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

// systemOverheadTokens 粗估请求信封的固定 token 开销：role/结构标签、分隔符、common wrapper。
// 凭经验取值（无标准库 tokenizer 无法精确）；宁高勿低，使压缩早触发而非晚触发。
const systemOverheadTokens = 400

// perToolSchemaTokens 粗估单个工具的 schema（name+description+parameters JSON）入请求的 token 数。
const perToolSchemaTokens = 60

// contextTrimToolChars 是 context 超限时把每条 tool 结果 content 压到的字符上限。
const contextTrimToolChars = 1000

// trimHistoryForContext 在端点返回 context_length_exceeded 时收紧历史，供 Run 单次重试。
// 策略按「可丢失性」排序：1. 清空所有 reasoning；2. 每条 tool 结果 content 压到 contextTrimToolChars。
// 不删消息：删 tool 消息会破坏 assistant.tool_calls / tool 配对，续跑会被端点 400。
func trimHistoryForContext(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Reasoning = ""
		if out[i].Role == roleTool {
			out[i].Content = truncate(strings.TrimSpace(out[i].Content), contextTrimToolChars, "…[context_trim]")
		}
	}
	return out
}

// estimateTokens 估算一次请求的 token 数，仅用于历史裁剪决策。启发式：CJK ≈ 1 token/2 字符，
// 其他 ≈ 1 token/4 字符。除 msgs 内容外，计入 system prompt 文本 + 固定请求开销（信封 + 工具 schema）。
func estimateTokens(msgs []Message, system string, tools []Tool) int {
	var nonCJK, cjk int
	add := func(s string) {
		for _, r := range s {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
				unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				cjk++
			} else {
				nonCJK++
			}
		}
	}
	for _, m := range msgs {
		add(m.Content)
		add(m.Reasoning)
		for _, tc := range m.ToolCalls {
			add(tc.Args)
		}
	}
	add(system)
	return nonCJK/4 + cjk/2 + systemOverheadTokens + perToolSchemaTokens*len(tools)
}

// contextKeepRecent 是 compactHistory/compactWithSummary 默认保留的最近轮数。
const contextKeepRecent = 6

// splitRounds 按「轮」切分 msgs：「带 tool_calls 的 assistant + 其后连续 tool 结果」为
// 一轮（成组，保 tool_calls/tool 配对）；user 与无 tool_calls 的 assistant 各自独立成轮。
func splitRounds(msgs []Message) [][]Message {
	var rounds [][]Message
	var cur []Message
	flush := func() {
		if len(cur) > 0 {
			rounds = append(rounds, cur)
			cur = nil
		}
	}
	for _, m := range msgs {
		if m.Role == roleTool && len(cur) > 0 {
			cur = append(cur, m) // tool 归属当前开启的 assistant(tool_calls) 轮
			continue
		}
		flush()
		cur = []Message{m}
		if len(m.ToolCalls) == 0 {
			flush() // user / 纯 assistant：独立成轮
		}
		// 带 tool_calls 的 assistant：cur 保持开启，吸收后续 tool 消息
	}
	flush()
	return rounds
}

func flatten(rounds [][]Message) []Message {
	var out []Message
	for _, r := range rounds {
		out = append(out, r...)
	}
	return out
}

// compactHistory 是摘要失败/无中段时的有损 fallback：保留「最早 1 轮 + 最近 keepRecent 轮」，
// 中段整轮剔除（保 tool 配对）。轮数不足 1+keepRecent 时原样返回（无需裁剪）。
func compactHistory(msgs []Message, keepRecent int) []Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs
	}
	out := append([]Message{}, rounds[0]...)
	out = append(out, flatten(rounds[len(rounds)-keepRecent:])...)
	return out
}

// trimRecentRounds 只保留最近 keepRecent 轮（丢 summary + 最早 + 全部旧历史），
// 是 compactHistory 仍超 window 时的最终有损裁剪（审查 v3 #4：避免循环烧请求）。
func trimRecentRounds(msgs []Message, keepRecent int) []Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= keepRecent {
		return msgs
	}
	return flatten(rounds[len(rounds)-keepRecent:])
}
