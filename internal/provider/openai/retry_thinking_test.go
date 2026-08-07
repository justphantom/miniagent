package openai

import "testing"

// isThinkingError（W2 收紧）：强信号字段名（reasoning_effort*）直接命中；弱信号需 thinking+unknown 组合，
// 防「unrecognized tool name」「unknown model」「unexpected argument type」等无关 400 被误判触发降级
// （错误归因 + 烧 2 次请求）。此前弱信号路径无单测（回归审查盲区）。
func TestIsThinkingError_StrongAndWeakSignals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"强信号 reasoning_effort", `{"error":{"message":"unknown parameter: reasoning_effort"}}`, true},
		{"强信号 reasoning_effort_level", `{"error":{"message":"unexpected reasoning_effort_level"}}`, true},
		{"弱信号 reasoning+unrecognized", `{"error":{"message":"unrecognized reasoning parameter"}}`, true},
		{"弱信号 thinking+unknown parameter", `{"error":{"message":"unknown parameter: thinking"}}`, true},
		{"无关 unrecognized tool name", `{"error":{"message":"unrecognized tool name: foo"}}`, false},
		{"无关 unknown model", `{"error":{"message":"unknown model: gpt-x"}}`, false},
		{"无关 unexpected argument type", `{"error":{"message":"unexpected argument type"}}`, false},
	}
	for _, c := range cases {
		if got := isThinkingError([]byte(c.body)); got != c.want {
			t.Errorf("%s: isThinkingError=%v, want %v（body=%s）", c.name, got, c.want, c.body)
		}
	}
}
