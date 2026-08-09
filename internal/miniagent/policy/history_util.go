package policy

import (
	"encoding/json"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"strings"
	"unicode"

	"github.com/justphantom/miniagent/internal/text"
)

// The following 4 token estimation constants are parameters of the core token estimation
// (EstimateTokens/schemaTokens) and belong to core. Moved out of context.go so the compaction
// subpackage can be migrated wholesale. SystemOverheadTokens is exported for compaction's estimateRoundTokens to reuse.
const SystemOverheadTokens = 400

// EnvelopePerMsgTokens / EnvelopePerToolCallTokens are linear token estimates for the request envelope
// (grow with the number of messages and tool_calls). Exported for compaction.estimateMessageTokensLocal
// to reuse the same metric (including envelope margin), same rationale as SystemOverheadTokens.
const (
	EnvelopePerMsgTokens      = 4
	EnvelopePerToolCallTokens = 20
)

// perToolSchemaTokens roughly estimates the number of tokens a single tool schema contributes to the
// request (a flat fallback used when schemaTokens serialization fails).
const perToolSchemaTokens = 60

// EstimateTokens estimates the token count for a single request, used only for history trim decisions.
// Heuristic: CJK ≈ 1 token/2 chars, others ≈ 1 token/4 chars. Beyond the msgs content, it also accounts
// for the system prompt text + request envelope + tool schemas. Exported for the compaction subpackage
// to reuse the same estimation metric (depends on the miniagent.Message/miniagent.Tool domain types, hence kept as a core export).
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

// schemaTokens estimates the fixed token overhead based on the actual JSON schema size of tools;
// on serialization failure it falls back to a flat estimate. Not cached: tools are a fixed small set
// (built-in tools + scripts), so marshal cost is negligible; caching keyed by slice pointer identity
// would, across Runs, collide under GC address reuse (different-content slices of the same length
// reuse the same first-element address) and return stale values — not worth it.
func schemaTokens(tools []miniagent.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	return schemaTokensCompute(tools)
}

// schemaTokensCompute is the uncached implementation of schemaTokens.
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

// estimateResponseTokens performs a local token estimate based on the response text/reasoning/tool
// args, used as a fallback for budget circuit-break when usage is zero.
func estimateResponseTokens(resp miniagent.Response) int {
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

// contextTrimToolChars is the upper limit (in characters) that each tool result content is compressed
// to when context is exceeded. Overridden at runtime via Limits.ContextTrimToolChars (<=0 uses this
// default), injected by NewDefaultOnLLMError.
const contextTrimToolChars = 2000

// trimHistoryForContext tightens history when the endpoint returns context_length_exceeded, allowing
// Run a single retry. No messages are deleted: deleting tool messages would break the
// assistant.tool_calls / tool pairing.
func trimHistoryForContext(msgs []miniagent.Message, contextTrim int) []miniagent.Message {
	if contextTrim <= 0 {
		contextTrim = contextTrimToolChars
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Reasoning = ""
		if out[i].Role == miniagent.RoleTool {
			out[i].Content = text.Truncate(strings.TrimSpace(out[i].Content), contextTrim, "…[context_trim]")
		}
	}
	return out
}
