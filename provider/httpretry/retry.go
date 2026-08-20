// Package httpretry holds the vendor-agnostic HTTP retry primitives used by
// the openai provider: the retry constants, the Retry-After
// header parser, the backoff capper, the context-aware sleep, and a
// parameterized retryable-status classifier.
package httpretry

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Retry applies only to transient failures (429/5xx + network errors), up to
// MaxRetries times. Endpoint 429/503 jitter self-heals within seconds, so 2
// attempts covers typical spikes; this avoids amplifying downstream pressure
// under a real outage (cascading failure).
const (
	MaxRetries     = 2
	RetryBaseDelay = 500 * time.Millisecond
	RetryMaxDelay  = 8 * time.Second // per-attempt backoff cap, includes the parsed Retry-After value
)

// ShouldRetryStatus reports whether an HTTP status code is a transient failure
// worth retrying. The common 429/5xx baseline is built in; extra appends
// vendor-specific transient codes (e.g. Anthropic's 529 overloaded_error) that
// have no stdlib constant and no cross-vendor equivalent.
func ShouldRetryStatus(code int, extra ...int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return slices.Contains(extra, code)
}

// ParseRetryAfter parses the Retry-After header: seconds (RFC 7231 §7.1.3) or
// an HTTP-date. It returns -1 (sentinel) when absent or unparseable, to
// distinguish an explicit "Retry-After: 0" — the latter means retry
// immediately. The return value is not capped (capping happens at the call
// site).
func ParseRetryAfter(h http.Header) time.Duration {
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
		// HTTP-date already in the past: semantically equivalent to "retry now",
		// return 0 (unlike -1 which goes through backoff).
		return 0
	}
	return -1
}

// CapRetryDelay applies an explicit Retry-After (>=0, including 0=immediate)
// taking precedence over exponential backoff, then caps it at RetryMaxDelay.
// A retryAfter < 0 means absent (use backoff).
func CapRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > RetryMaxDelay {
		backoff = RetryMaxDelay
	}
	return backoff
}

// SleepCtx waits for delay or until ctx is canceled; if ctx becomes ready
// first it returns ctx.Err().
func SleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
