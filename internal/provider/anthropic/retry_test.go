package anthropic

import (
	"net/http"
	"testing"
)

// Provider-specific retry classification: the 529 overloaded_error is Anthropic's
// canonical transient under load (no stdlib constant; the common 429/5xx baseline
// plus ParseRetryAfter/CapRetryDelay/SleepCtx lives in the httpretry package, whose
// baseline + extra-parameterization is unit-tested there).

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
		{529, true}, // Anthropic overloaded_error (no stdlib constant)
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
