package policy

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	// 纯 ASCII：4 字符 ≈ 1 token；空 system + 无工具时 = 内容 + systemOverhead + 每条消息信封。
	if n := EstimateTokens([]miniagent.Message{{Role: "user", Content: "abcdefgh"}}, "", nil); n != 2+SystemOverheadTokens+EnvelopePerMsgTokens {
		t.Errorf("ascii 8 chars = %d, want %d", n, 2+SystemOverheadTokens+EnvelopePerMsgTokens)
	}
	// 纯中文：2 字符 ≈ 1 token
	if n := EstimateTokens([]miniagent.Message{{Role: "user", Content: "四个汉字"}}, "", nil); n != 2+SystemOverheadTokens+EnvelopePerMsgTokens {
		t.Errorf("cjk 4 chars = %d, want %d", n, 2+SystemOverheadTokens+EnvelopePerMsgTokens)
	}
	// tool_calls.Args 计入估算；每个 tool_call 额外计信封（嵌套 function 对象）。
	if n := EstimateTokens([]miniagent.Message{{Role: "assistant", ToolCalls: []miniagent.ToolCall{{Args: "abcd"}}}}, "", nil); n != 1+SystemOverheadTokens+EnvelopePerMsgTokens+EnvelopePerToolCallTokens {
		t.Errorf("args 4 chars = %d, want %d", n, 1+SystemOverheadTokens+EnvelopePerMsgTokens+EnvelopePerToolCallTokens)
	}
}

// estimateTokens 失明：system prompt 内容 + 工具 schema 固定开销须计入，否则压缩触发偏晚。
// 用 delta 断言，使测试不依赖具体常量取值。
func TestEstimateTokens_Overhead(t *testing.T) {
	msgs := []miniagent.Message{{Role: "user", Content: "abcdefgh"}}
	base := EstimateTokens(msgs, "", nil)
	if got := EstimateTokens(msgs, "abcd", nil) - base; got != 1 {
		t.Errorf("system 4 chars should add 1 token, got delta %d", got)
	}
	small := EstimateTokens(msgs, "", []miniagent.Tool{{Name: "t", Description: "abcd"}}) - base
	large := EstimateTokens(msgs, "", []miniagent.Tool{{Name: "t", Description: strings.Repeat("a", 400)}}) - base
	if large <= small {
		t.Errorf("schema estimate should grow with description length: small=%d large=%d", small, large)
	}
	if small <= 0 {
		t.Errorf("non-empty tool schema should add tokens: small=%d", small)
	}
	if got := EstimateTokens(msgs, strings.Repeat("a", 40), nil) - base; got != 10 {
		t.Errorf("system 40 chars should add 10 tokens, got delta %d", got)
	}
}
