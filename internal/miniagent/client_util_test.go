package miniagent

import "testing"

// §P1-C isContextLengthError：多厂商超限措辞正例 + throttling/空/限流反例。
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
		if !isContextLengthError([]byte(body)) {
			t.Errorf("正例应判 true: %q", body)
		}
	}

	negatives := []string{
		"", // 空 body 不误判
		"ThrottlingException: Too many tokens, please wait", // Bedrock 限流，非超限
		"rate limit exceeded",
		"too many requests, slow down",
		"Service unavailable: maintenance",
		"internal server error",
	}
	for _, body := range negatives {
		if isContextLengthError([]byte(body)) {
			t.Errorf("反例应判 false: %q", body)
		}
	}
}

// §P1-C isSilentContextOverflow：stop 超窗 / length+output=0+input≥99% 两类静默溢出 + 边界。
func TestIsSilentContextOverflow(t *testing.T) {
	cases := []struct {
		name          string
		finish        string
		input, output int
		window        int
		want          bool
	}{
		{"stop_over_window", finishStop, 1500, 10, 1000, true},
		{"stop_under_window", finishStop, 900, 10, 1000, false},
		{"length_zero_output_99pct", finishLength, 990, 0, 1000, true}, // 990>=1000*99/100=990
		{"length_zero_output_98pct", finishLength, 980, 0, 1000, false},
		{"length_with_output", finishLength, 1500, 50, 1000, false}, // output!=0 → 非 Case3
		{"window_zero", finishStop, 1500, 10, 0, false},
		{"input_zero", finishStop, 0, 10, 1000, false},
		{"other_finish", "tool_calls", 1500, 10, 1000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := Response{FinishReason: c.finish, Usage: Usage{InputTokens: c.input, OutputTokens: c.output}}
			if got := isSilentContextOverflow(resp, c.window); got != c.want {
				t.Errorf("isSilentContextOverflow = %v, want %v", got, c.want)
			}
		})
	}
}
