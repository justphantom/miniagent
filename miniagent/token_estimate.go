package miniagent

import (
	"encoding/json"
	"unicode"

	"github.com/justphantom/miniagent/text"
)

// Token estimation lives in the core, not the policy subpackage: it is a pure request-side metric
// (no strategy) shared by compaction (FitHistory threshold / jointTailBudget) and policy (OnBudget
// zero-usage fallback). Keeping it in core lets compaction depend on core alone, eliminating the
// compaction→policy cross-subpackage edge. Heuristic: CJK ≈ 1 token/2 chars, others ≈ 1 token/4 chars.

// SystemOverheadTokens is the fixed per-request token overhead of the system prompt + request envelope.
const SystemOverheadTokens = 400

const (
	// EnvelopePerMsgTokens is the fixed per-message envelope overhead in the estimate.
	EnvelopePerMsgTokens = 4
	// EnvelopePerToolCallTokens is the fixed per-tool-call envelope overhead in the estimate.
	EnvelopePerToolCallTokens = 20
	// perToolSchemaTokens roughly estimates the tokens one tool schema contributes (fallback on marshal failure).
	perToolSchemaTokens = 60
)

// EstimateTokens estimates the token count for a single request, used for history trim decisions.
// Beyond the msgs content, it accounts for the system prompt text + request envelope + tool schemas.
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
		add(m.ReasoningState)
		toolCalls += len(m.ToolCalls)
		for _, tc := range m.ToolCalls {
			add(tc.Args)
		}
	}
	add(system)
	return nonCJK/4 + cjk/2 + SystemOverheadTokens +
		EnvelopePerMsgTokens*len(msgs) + EnvelopePerToolCallTokens*toolCalls +
		schemaTokens(tools)
}

// schemaTokens estimates the fixed token overhead from the actual JSON schema size of tools;
// on serialization failure it falls back to a flat estimate. Not cached: tools are a fixed small set,
// so marshal cost is negligible; caching keyed by slice pointer identity would, across Runs, collide
// under GC address reuse and return stale values — not worth it.
func schemaTokens(tools []Tool) int {
	if len(tools) == 0 {
		return 0
	}
	return schemaTokensCompute(tools)
}

// schemaTokensCompute is the uncached implementation of schemaTokens.
func schemaTokensCompute(tools []Tool) int {
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
func EstimateResponseTokens(resp Response) int {
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

// TrimForHistory trims a tool result down to limit chars before it enters history; limit<=0 uses the default
// cap MaxToolResultInHistory. split=true (shell/grep-like tools whose tail is critical) uses head+tail
// segmented truncation retaining the tail error conclusion; otherwise head-only. The C-2 context downgrade
// reuses the same trimming semantics. It lives in core because it is a pure string operation with no strategy,
// and compaction/policy both depend on it — eliminating the compaction→policy and policy→text edges.
func TrimForHistory(s string, limit int, split bool) string {
	if limit <= 0 {
		limit = MaxToolResultInHistory
	}
	if split {
		return text.TruncateHeadTail(s, limit, "…[middle omitted]")
	}
	return text.Truncate(s, limit, "…[tool_result truncated]")
}
