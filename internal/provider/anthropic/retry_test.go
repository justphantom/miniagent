package anthropic

import (
	"net/http"
	"testing"
	"time"
)

func TestShouldRetryStatus(t *testing.T) {
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
		if got := shouldRetryStatus(c.code); got != c.want {
			t.Errorf("shouldRetryStatus(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		hdr  string
		want time.Duration
	}{
		{"", -1},
		{"0", 0},
		{"2", 2 * time.Second},
		{"junk", -1},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.hdr != "" {
			h.Set("Retry-After", c.hdr)
		}
		if got := parseRetryAfter(h); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.hdr, got, c.want)
		}
	}
}

func TestCapRetryDelay(t *testing.T) {
	if got := capRetryDelay(retryBaseDelay, -1); got != retryBaseDelay {
		t.Errorf("capRetryDelay(backoff,-1) = %v, want backoff", got)
	}
	if got := capRetryDelay(retryBaseDelay, 0); got != 0 {
		t.Errorf("capRetryDelay(backoff,0) = %v, want 0 (Retry-After precedence)", got)
	}
	if got := capRetryDelay(retryMaxDelay*2, -1); got != retryMaxDelay {
		t.Errorf("capRetryDelay(>max,-1) = %v, want retryMaxDelay cap", got)
	}
}

func TestIsThinkingError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"enabled on 4.7+ model", `{"error":{"message":"\"thinking.type.enabled\" is not supported"}}`, true},
		{"adaptive on 4.5- model", `{"error":{"message":"\"thinking.type.adaptive\" is not supported"}}`, true},
		{"generic budget unsupported", `{"error":{"message":"thinking budget_tokens is not supported by this model"}}`, true},
		{"unrecognized thinking param", `{"error":{"message":"unrecognized parameter: thinking"}}`, true},
		{"unrelated unknown tool", `{"error":{"message":"unknown tool name"}}`, false},
		{"unrelated invalid model", `{"error":{"message":"invalid model id"}}`, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := isThinkingError([]byte(c.raw)); got != c.want {
			t.Errorf("isThinkingError[%s] = %v, want %v", c.name, got, c.want)
		}
	}
}
