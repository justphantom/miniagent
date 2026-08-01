package miniagent

import (
	"strings"
	"unicode"
)

// contextTrimToolChars 是 context 超限时把每条 tool 结果 content 压到的字符上限：
// 远小于常规上限（maxToolResultInHistory / read 的 ResultLimit），确保单次收紧
// 足以让重试通过。字符数是 token 的启发式近似（标准库无 tokenizer），仅用于
// "砍多少"的估算——精确计费仍以端点 usage 为准。
const contextTrimToolChars = 1000

// trimHistoryForContext 在端点返回 context_length_exceeded 时收紧历史，供 Run
// 单次重试。策略按"可丢失性"排序：
//  1. 清空所有 reasoning（思考链最可丢，code/content 不可丢）；
//  2. 每条 tool 结果 content 压到 contextTrimToolChars。
//
// 不删消息：删 tool 消息会破坏 assistant.tool_calls / tool 的配对，续跑会被端点
// 400（validateToolPairing 也会拦）。返回新 slice，不改调用方输入。
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

// estimateTokens 估算 msgs 的 token 数，仅用于历史裁剪决策（context 接近窗口时触发）。
// 启发式：CJK 字符 ≈ 1 token/2 字符，其他 ≈ 1 token/4 字符。字符数是 token 的近似
// （标准库无 tokenizer），仅用于「砍多少」估算——精确计费仍以端点 usage 为准。
func estimateTokens(msgs []Message) int {
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
	return nonCJK/4 + cjk/2
}

// contextKeepRecent 是 compactHistory 默认保留的最近轮数。
const contextKeepRecent = 6

// compactHistory 保留「最早 1 轮 + 最近 keepRecent 轮」，中段整轮剔除。
// 「轮」：带 tool_calls 的 assistant + 其后连续的 tool 结果消息为一轮（成组删除，
// 保证 tool_calls/tool 配对不被破坏）；user 与无 tool_calls 的 assistant 各自独立
// 成轮。返回新 slice，不改入参。轮数不足 1+keepRecent 时原样返回（无需裁剪）。
func compactHistory(msgs []Message, keepRecent int) []Message {
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
	if len(rounds) <= 1+keepRecent {
		return msgs
	}
	out := append([]Message{}, rounds[0]...)
	for _, r := range rounds[len(rounds)-keepRecent:] {
		out = append(out, r...)
	}
	return out
}
