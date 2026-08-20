package openai

import (
	"strings"

	"github.com/justphantom/miniagent/internal/provider/httpretry"
)

// Retry semantics — the constants (MaxRetries / RetryBaseDelay / RetryMaxDelay),
// the Retry-After / backoff / sleep helpers, and the common 429/5xx
// retryable-status baseline — live in the vendor-agnostic httpretry package.
// This file holds only OpenAI-specific 400-body classification.

// isContextLengthError is identified in core (overflow.go, miniagent.IsContextLengthError):
// 24 regexes + 4 exclusions (§P1-C). This package calls miniagent.IsContextLengthError to avoid forking.

// shouldRetryStatus delegates to the common baseline; OpenAI has no
// vendor-specific transient status beyond 429/5xx.
func shouldRetryStatus(code int) bool {
	return httpretry.ShouldRetryStatus(code)
}

// isThinkingError heuristically identifies a 400 indicating the thinking parameter (reasoning_effort,
// etc.) is unsupported: vendor wording varies ("reasoning_effort"/"unknown parameter"/"unrecognized").
// A strong signal (the field name) hits directly; a weak signal requires both thinking/reasoning
// semantics + unrecognized-parameter wording — this prevents unrelated 400s such as "unrecognized
// tool name" or "unknown model" from being misclassified as thinking-unsupported (wrong attribution
// + 2 wasted requests). A misclassification only triggers a single thinking-less retry (review v2 #7).
func isThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning_effort_level") {
		return true
	}
	hasThinking := strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking")
	hasUnknown := strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unexpected argument")
	return hasThinking && hasUnknown
}
