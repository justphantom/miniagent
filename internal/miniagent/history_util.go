package miniagent

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/justphantom/miniagent/internal/text"
)

// 以下 4 个 token 估算常量是核心 token 估算（EstimateTokens/schemaTokens）的参数，属核心；
// 从 context.go 迁出以使压缩子包可整体外迁。SystemOverheadTokens 导出供压缩的 estimateRoundTokens 复用。
const SystemOverheadTokens = 400

// envelopePerMsgTokens / envelopePerToolCallTokens 是请求信封的线性化 token 估算（随消息数与 tool_call 数增长）。
const (
	envelopePerMsgTokens      = 4
	envelopePerToolCallTokens = 20
)

// perToolSchemaTokens 粗估单个工具 schema 入请求的 token 数（schemaTokens 序列化失败的 flat 兜底）。
const perToolSchemaTokens = 60

// EstimateTokens 估算一次请求的 token 数，仅用于历史裁剪决策。启发式：CJK ≈ 1 token/2 字符，
// 其他 ≈ 1 token/4 字符。除 msgs 内容外，计入 system prompt 文本 + 请求信封 + 工具 schema。
// 导出供压缩子包（compaction）复用同一估算口径（依赖 Message/Tool 领域类型，故留 core 导出）。
func EstimateTokens(msgs []Message, system string, tools []Tool) int {
	var nonCJK, cjk int
	var toolCalls int
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
		toolCalls += len(m.ToolCalls)
		for _, tc := range m.ToolCalls {
			add(tc.Args)
		}
	}
	add(system)
	return nonCJK/4 + cjk/2 + SystemOverheadTokens +
		envelopePerMsgTokens*len(msgs) + envelopePerToolCallTokens*toolCalls +
		schemaTokens(tools)
}

// schemaTokens 按 tools 实际 JSON schema 体积估算固定 token 开销；序列化失败回落 flat 估算。
func schemaTokens(tools []Tool) int {
	if len(tools) == 0 {
		return 0
	}
	var nonCJK, cjk int
	for _, t := range tools {
		b, err := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
		if err != nil {
			return perToolSchemaTokens * len(tools)
		}
		for _, r := range string(b) {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
				unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				cjk++
			} else {
				nonCJK++
			}
		}
	}
	return nonCJK/4 + cjk/2
}

// estimateResponseTokens 基于响应文本/思考链/工具参数做本地 token 估算，供零 usage 预算熔断 fallback。
func estimateResponseTokens(resp Response) int {
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
	add(resp.Text)
	add(resp.Reasoning)
	for _, tc := range resp.ToolCalls {
		add(tc.Args)
	}
	return nonCJK/4 + cjk/2
}

// contextTrimToolChars 是 context 超限时把每条 tool 结果 content 压到的字符上限。
// 运行时经 Limits.ContextTrimToolChars 覆盖（<=0 用此默认），由 NewDefaultOnLLMError 注入。
const contextTrimToolChars = 2000

// trimHistoryForContext 在端点返回 context_length_exceeded 时收紧历史，供 Run 单次重试。
// 不删消息：删 tool 消息会破坏 assistant.tool_calls / tool 配对。
func trimHistoryForContext(msgs []Message, contextTrim int) []Message {
	if contextTrim <= 0 {
		contextTrim = contextTrimToolChars
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Reasoning = ""
		if out[i].Role == RoleTool {
			out[i].Content = text.Truncate(strings.TrimSpace(out[i].Content), contextTrim, "…[context_trim]")
		}
	}
	return out
}
