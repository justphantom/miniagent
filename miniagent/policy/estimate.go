package policy

import (
	"encoding/json"
	"unicode"

	miniagent "github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/text"
)

// Token estimation lives in the policy subpackage: it is a strategy-level heuristic (no core
// consensus), used by the default OnBudget zero-usage fallback and wired into compaction via
// the EstimateTokens callback. Keeping it in policy eliminates the core→policy dependency
// while keeping the heuristic available for default assembly.
// Heuristic: CJK ≈ 1 token/2 chars, others ≈ 1 token/4 chars.

// systemOverheadTokens is the fixed per-request token overhead of the system prompt + request envelope.
const systemOverheadTokens = 400

// Exported mirrors of the default-estimate constants, so the compaction package can pin its marginal math
// (budget_tail.go / estimateMessageTokensLocal) to the same contract via a consistency test — if the default
// overheads change, the mirror comparison fails instead of silently drifting the tail budget.
const (
	SystemOverheadTokens      = systemOverheadTokens
	EnvelopePerMsgTokens      = envelopePerMsgTokens
	EnvelopePerToolCallTokens = envelopePerToolCallTokens
)

const (
	envelopePerMsgTokens      = 4
	envelopePerToolCallTokens = 20
	perToolSchemaTokens       = 60
)

// EstimateTokens estimates the token count for a single request, used for history trim decisions.
// Exported for the cmd layer to wire into compaction's EstimateTokens callback.
func EstimateTokens(msgs []miniagent.Message, system string, tools []miniagent.Tool) int {
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
		add(m.ReasoningState)
		toolCalls += len(m.ToolCalls)
		for _, tc := range m.ToolCalls {
			add(tc.Args)
		}
	}
	add(system)
	return nonCJK/4 + cjk/2 + systemOverheadTokens +
		envelopePerMsgTokens*len(msgs) + envelopePerToolCallTokens*toolCalls +
		schemaTokens(tools)
}

// schemaTokens estimates the fixed token overhead from the actual JSON schema size of tools.
func schemaTokens(tools []miniagent.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	return schemaTokensCompute(tools)
}

func schemaTokensCompute(tools []miniagent.Tool) int {
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

// EstimateResponseTokens performs a local token estimate of a response (text/reasoning/tool args),
// used as a fallback for budget circuit-break when usage is zero.
func EstimateResponseTokens(resp miniagent.Response) int {
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
	add(resp.ReasoningState)
	for _, tc := range resp.ToolCalls {
		add(tc.Args)
	}
	return nonCJK/4 + cjk/2
}

// TrimForHistory trims a tool result down to limit chars before it enters history; limit<=0 uses the
// default cap MaxToolResultInHistory. split=true uses head+tail segmented truncation retaining the tail
// error conclusion; otherwise head-only.
func TrimForHistory(s string, limit int, split bool) string {
	if limit <= 0 {
		limit = miniagent.MaxToolResultInHistory
	}
	if split {
		return text.TruncateHeadTail(s, limit, "…[middle omitted]")
	}
	return text.Truncate(s, limit, "…[tool_result truncated]")
}
