package openai

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retry: applies only to transient failures (429/5xx + network errors), up to maxRetries times.
// Endpoint 429/503 jitter self-heals within seconds, so 2 attempts covers typical spikes; this
// avoids amplifying downstream pressure under a real outage (cascading failure).
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
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// isContextLengthError is identified in core (overflow.go, miniagent.IsContextLengthError):
// 24 regexes + 4 exclusions (§P1-C). This package calls miniagent.IsContextLengthError to avoid forking.

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

// parseRetryAfter parses the Retry-After header: seconds (RFC 7231 §7.1.3) or an HTTP-date.
// Returns -1 (sentinel) when absent or unparseable, to distinguish an explicit "Retry-After: 0" —
// the latter means retry immediately. The return value is not capped (capping happens at the call site).
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
		// HTTP-date already in the past: semantically equivalent to "retry now", return 0 (unlike -1
		// which goes through backoff). P3-3.
		return 0
	}
	return -1
}

// capRetryDelay: an explicit Retry-After (>=0, including 0=immediate) takes precedence over
// exponential backoff, then is capped at retryMaxDelay. retryAfter<0 means absent. Shared by the
// retry loops of ChatClient.Do and StreamClient.DoStream (P2-4).
func capRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > retryMaxDelay {
		backoff = retryMaxDelay
	}
	return backoff
}

// sleepCtx waits for delay or until ctx is canceled; if ctx becomes ready first it returns ctx.Err().
// Shared by the retry loops of ChatClient.Do and StreamClient.DoStream.
func sleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
