package miniagent

import "strings"

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
