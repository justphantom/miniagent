package responses

import "testing"

func TestIsThinkingError(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"message":"Unsupported reasoning.effort value"}}`: true,
		`{"error":{"message":"unknown parameter: reasoning"}}`:       true,
		`{"error":{"message":"unknown model"}}`:                      false,
	}
	for body, want := range cases {
		if got := isThinkingError([]byte(body)); got != want {
			t.Errorf("isThinkingError(%s)=%v want %v", body, got, want)
		}
	}
}
