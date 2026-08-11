package httpretry

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestShouldRetryStatus_CommonBaseline(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusOK, false},
	}
	for _, c := range cases {
		if got := ShouldRetryStatus(c.code); got != c.want {
			t.Errorf("ShouldRetryStatus(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

// Vendor-specific codes (no stdlib constant) are honored only when passed via extra.
func TestShouldRetryStatus_ExtraCodes(t *testing.T) {
	if !ShouldRetryStatus(529, 529) { // Anthropic overloaded_error
		t.Error("ShouldRetryStatus(529, 529) = false, want true")
	}
	if ShouldRetryStatus(529) { // 529 is not in the common baseline
		t.Error("ShouldRetryStatus(529) = true, want false (not in baseline)")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", -1},
		{"explicit-zero", "0", 0},
		{"seconds", "2", 2 * time.Second},
		{"garbage", "not-a-date", -1},
		// An HTTP-date already in the past is semantically equivalent to retrying
		// immediately, returning 0 (rather than -1 which would fall back to backoff).
		{"past-http-date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := ParseRetryAfter(h); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCapRetryDelay(t *testing.T) {
	if got := CapRetryDelay(RetryBaseDelay, -1); got != RetryBaseDelay {
		t.Errorf("CapRetryDelay(backoff,-1) = %v, want backoff", got)
	}
	if got := CapRetryDelay(RetryBaseDelay, 0); got != 0 {
		t.Errorf("CapRetryDelay(backoff,0) = %v, want 0 (Retry-After precedence)", got)
	}
	if got := CapRetryDelay(RetryMaxDelay*2, -1); got != RetryMaxDelay {
		t.Errorf("CapRetryDelay(>max,-1) = %v, want RetryMaxDelay cap", got)
	}
}

func TestSleepCtx_CancelledByContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// A 1s sleep must be interrupted by the 10ms context deadline.
	if err := SleepCtx(ctx, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("SleepCtx err = %v, want DeadlineExceeded", err)
	}
}
