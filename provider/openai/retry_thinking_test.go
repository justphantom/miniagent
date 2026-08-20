package openai

import "testing"

// isThinkingError (W2 tightening): a strong signal field name (reasoning_effort*) hits directly; a
// weak signal requires a thinking+unknown combination, preventing unrelated 400s such as
// "unrecognized tool name", "unknown model", or "unexpected argument type" from being misclassified
// and triggering a downgrade (wrong attribution + 2 wasted requests). The weak-signal path had no
// unit test before (a regression-review blind spot).
func TestIsThinkingError_StrongAndWeakSignals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"strong signal reasoning_effort", `{"error":{"message":"unknown parameter: reasoning_effort"}}`, true},
		{"strong signal reasoning_effort_level", `{"error":{"message":"unexpected reasoning_effort_level"}}`, true},
		{"weak signal reasoning+unrecognized", `{"error":{"message":"unrecognized reasoning parameter"}}`, true},
		{"weak signal thinking+unknown parameter", `{"error":{"message":"unknown parameter: thinking"}}`, true},
		{"unrelated unrecognized tool name", `{"error":{"message":"unrecognized tool name: foo"}}`, false},
		{"unrelated unknown model", `{"error":{"message":"unknown model: gpt-x"}}`, false},
		{"unrelated unexpected argument type", `{"error":{"message":"unexpected argument type"}}`, false},
	}
	for _, c := range cases {
		if got := isThinkingError([]byte(c.body)); got != c.want {
			t.Errorf("%s: isThinkingError=%v, want %v (body=%s)", c.name, got, c.want, c.body)
		}
	}
}
