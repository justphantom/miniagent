// Package looptest 提供循环核心（package miniagent）与策略子包（package policy）
// 共享的 LLM 测试桩：经 FakeTransport 走 HTTP，自带 OpenAI 兼容 wire 构造/解析/重试
// （openai 包逻辑的测试子集副本），使循环测试不依赖 openai 包——避免 core_test → openai → core
// 的测试环。仅测试引用，不进生产二进制（虽非 _test.go，但导出符号只被各子包 _test 消费）。
package looptest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// TextResponse 构造非流式 chat completions JSON：单条 choice，纯文本回复，固定 usage {1,1}。
func TextResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// ToolResponse 构造非流式 chat completions JSON：单条 choice 带 tool_calls（content 恒空）。
func ToolResponse(calls ...miniagent.ToolCall) string {
	tcs := make([]string, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}

// LastToolMessage 返回 msgs 中最后一条 role=tool 的消息（测试辅助）。
func LastToolMessage(t *testing.T, msgs []miniagent.Message) miniagent.Message {
	t.Helper()
	for idx := range slices.Backward(msgs) {
		if msgs[idx].Role == miniagent.RoleTool {
			return msgs[idx]
		}
	}
	t.Fatalf("no tool message in msgs: %+v", msgs)
	return miniagent.Message{}
}
