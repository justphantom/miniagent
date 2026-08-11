package anthropic

import (
	"strings"

	"github.com/justphantom/miniagent/internal/provider/httpretry"
)

// Retry semantics — the constants (MaxRetries / RetryBaseDelay / RetryMaxDelay)
// and the Retry-After / backoff / sleep helpers — live in the vendor-agnostic
// httpretry package, shared verbatim with the openai provider. This file holds
// only Anthropic-specific classification: the retryable-status set (Anthropic
// adds 529 overloaded_error to the common 429/5xx baseline) and the
// thinking-error 400-body heuristic.

// shouldRetryStatus is the Anthropic retryable-status set: the common 429/5xx
// baseline plus 529 (overloaded_error) — the canonical Anthropic transient under
// load (Retryable=Yes in their error reference, retried by the official SDKs). No
// Go stdlib http.Status constant exists, and OpenAI has no equivalent, so this
// code is Anthropic-specific.
func shouldRetryStatus(code int) bool {
	return httpretry.ShouldRetryStatus(code, 529)
}

// isThinkingError identifies a 400 indicating the thinking parameter is the wrong shape for the target
// model family. Anthropic extended-thinking is model-family-dependent in 2026: Claude 4.7+ (Sonnet 5,
// Opus 4.8/5, Fable 5) REJECT thinking.type:"enabled"; Claude 4.5 and earlier REJECT thinking.type:"adaptive".
// A thinking.map configured with the wrong shape therefore surfaces as a 400 carrying one of these strong
// signals. callLLMWithDowngrade (loop_extra.go) uses this to drop thinking and retry once. The weak-signal
// branch guards against unfamiliar proxy wording without misclassifying unrelated 400s (e.g. "unknown tool").
func isThinkingError(raw []byte) bool {
	s := string(raw)
	if strings.Contains(s, `"thinking.type.enabled" is not supported`) { // 4.7+ rejects enabled
		return true
	}
	if strings.Contains(s, `"thinking.type.adaptive" is not supported`) { // 4.5- rejects adaptive
		return true
	}
	lower := strings.ToLower(s)
	hasThinking := strings.Contains(lower, "thinking") || strings.Contains(lower, "budget_tokens")
	hasUnknown := strings.Contains(lower, "is not supported") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unknown parameter")
	return hasThinking && hasUnknown
}
