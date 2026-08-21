package policy

import (
	miniagent "github.com/justphantom/miniagent/miniagent" // Explicit alias: many types clash with package-level names.
	"strings"

	"github.com/justphantom/miniagent/text"
)

// Token estimation (EstimateTokens / EstimateResponseTokens / SystemOverheadTokens / envelope
// constants / TrimForHistory) lives in the core (miniagent/token_estimate.go): it is a pure
// request-side metric with no strategy, shared by compaction and policy. Keeping it in core lets
// compaction depend on core alone — this file retains only the policy-side trim logic.

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
		out[i].ReasoningState = ""
		if out[i].Role == miniagent.RoleTool {
			out[i].Content = text.Truncate(strings.TrimSpace(out[i].Content), contextTrim, "…[context_trim]")
		}
	}
	return out
}
