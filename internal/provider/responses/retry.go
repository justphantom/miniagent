package responses

import "strings"

func isThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "reasoning.effort") || strings.Contains(lower, "unsupported reasoning effort") {
		return true
	}
	hasReasoning := strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking")
	hasUnknown := strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unexpected argument")
	return hasReasoning && hasUnknown
}
