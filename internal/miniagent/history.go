package miniagent

import (
	"strings"
	"unicode"
)

// systemOverheadTokens 粗估请求信封的固定 token 开销：role/结构标签、分隔符、common wrapper。
// 凭经验取值（无标准库 tokenizer 无法精确）；宁高勿低，使压缩早触发而非晚触发。
const systemOverheadTokens = 400

// perToolSchemaTokens 粗估单个工具的 schema（name+description+parameters JSON）入请求的 token 数。
// 九件套工具完整 schema 实测约 2-5K tokens，平均约 60/工具。仅用于压缩触发时机的启发式估算。
const perToolSchemaTokens = 60

// contextTrimToolChars 是 context 超限时把每条 tool 结果 content 压到的字符上限：
// 远小于常规上限（maxToolResultInHistory / read 的 ResultLimit），确保单次收紧足以让
// 重试通过。字符数是 token 的启发式近似（标准库无 tokenizer），仅用于「砍多少」估算。
const contextTrimToolChars = 1000

// trimHistoryForContext 在端点返回 context_length_exceeded 时收紧历史，供 Run 单次重试。
// 策略按「可丢失性」排序：1. 清空所有 reasoning；2. 每条 tool 结果 content 压到 contextTrimToolChars。
// 不删消息：删 tool 消息会破坏 assistant.tool_calls / tool 配对，续跑会被端点 400。
// 返回新 slice，不改调用方输入。
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
// 其他 ≈ 1 token/4 字符。除 msgs 内容外，计入 system prompt 文本 + 固定请求开销（信封 + 工具 schema）：
// 之前仅看 msgs 内容、不计 system 与 tools schema（~2-5K tokens），导致压缩触发偏晚（审查 P2 失明）。
// 精确计费仍以端点 usage 为准；常量凭经验取值。
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
	// system prompt 之前完全忽略；工具 schema 是固定开销，按工具数线性估。
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
