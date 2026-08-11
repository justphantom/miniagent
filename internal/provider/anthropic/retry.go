package anthropic

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retry: applies only to transient failures (429/5xx + network errors), up to maxRetries times.
// Vendor-agnostic, HTTP-status-code level — mirrored verbatim from the openai provider (the constants
// and helpers are not wire-specific), so the two providers share identical retry semantics.
const (
	maxRetries     = 2
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second // per-attempt backoff cap, includes the parsed Retry-After value
)

func shouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529:                            // Anthropic overloaded_error — the canonical Anthropic transient under load (Retryable=Yes in their error
		// reference, retried by the official SDKs). No Go stdlib http.Status constant exists, and OpenAI has no
		// equivalent, so this is Anthropic-specific (NOT mirrored from the openai provider's retry.go).
		return true
	}
	return false
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

// parseRetryAfter parses the Retry-After header: seconds (RFC 7231 §7.1.3) or an HTTP-date.
// Returns -1 (sentinel) when absent or unparseable, to distinguish an explicit "Retry-After: 0"
// (retry immediately). The return value is not capped (capping happens at the call site).
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return -1
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0 // HTTP-date in the past ≡ retry now
	}
	return -1
}

// capRetryDelay: an explicit Retry-After (>=0, including 0=immediate) takes precedence over exponential
// backoff, then is capped at retryMaxDelay. retryAfter<0 means absent.
func capRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > retryMaxDelay {
		backoff = retryMaxDelay
	}
	return backoff
}

// sleepCtx waits for delay or until ctx is canceled.
func sleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
