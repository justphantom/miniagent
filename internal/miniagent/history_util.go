package miniagent

import (
	"strings"
	"unicode"
)

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

// contextTrimToolChars 是 context 超限时把每条 tool 结果 content 压到的字符上限。
// 2000 与 maxToolResultInHistory 持平：1000 过紧，关键错误信息（如 go build 输出）可能被截断。
const contextTrimToolChars = 2000

// contextTrimToolCharsOverride 允许配置覆盖内置默认；nil 用常量默认。
var contextTrimToolCharsOverride int

// SetContextTrimToolChars 覆盖 context 超限时 tool 结果压缩上限；测试用，正常流程由 Resolve 调用。
func SetContextTrimToolChars(n int) {
	if n > 0 {
		contextTrimToolCharsOverride = n
	}
}

func getContextTrimToolChars() int {
	if contextTrimToolCharsOverride > 0 {
		return contextTrimToolCharsOverride
	}
	return contextTrimToolChars
}

// trimHistoryForContext 在端点返回 context_length_exceeded 时收紧历史，供 Run 单次重试。
// 策略按「可丢失性」排序：1. 清空所有 reasoning；2. 每条 tool 结果 content 压到 contextTrimToolChars。
// 不删消息：删 tool 消息会破坏 assistant.tool_calls / tool 配对，续跑会被端点 400。
func trimHistoryForContext(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Reasoning = ""
		if out[i].Role == roleTool {
			out[i].Content = truncate(strings.TrimSpace(out[i].Content), getContextTrimToolChars(), "…[context_trim]")
		}
	}
	return out
}
