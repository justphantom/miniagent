package openai

import (
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// §P1-C isContextLengthError: positive cases of multi-vendor context-length-exceeded wording + throttling/empty/rate-limit negatives.
func TestIsContextLengthError(t *testing.T) {
	positives := []string{
		"This model's maximum context length is 200000 tokens",
		"prompt is too long: 213462 tokens > 200000 maximum",
		"Your input exceeds the context window",
		"Please reduce the length of the messages",
		"context_length_exceeded",
		"too many tokens in request",
		"token limit exceeded",
		"The input (5000 tokens) is longer than the model's context length (4096 tokens)",
		"request_too_large",
		"model_context_window_exceeded",
		"prompt too long; exceeded max context length",
		"Requested token count exceeds the model's maximum context length of 131072 tokens",
		"too large for model with 32768 maximum context length",
		"Range of input length should be [1, 8192]",
		"input token count (1196265) exceeds the maximum",
	}
	for _, body := range positives {
		if !miniagent.IsContextLengthError([]byte(body)) {
			t.Errorf("positive case should be true: %q", body)
		}
	}

	negatives := []string{
		"", // empty body must not be misclassified
		"ThrottlingException: Too many tokens, please wait", // Bedrock rate limit, not context-length
		"rate limit exceeded",
		"too many requests, slow down",
		"Service unavailable: maintenance",
		"internal server error",
	}
	for _, body := range negatives {
		if miniagent.IsContextLengthError([]byte(body)) {
			t.Errorf("negative case should be false: %q", body)
		}
	}
}
