package openai

import (
	"strings"
	"testing"
)

// parseSSE 的 W1 守卫：provider 漏发 index 致多个 tool_call 归并到同 index 时报错，不静默合并
// （回归审查盲区：守卫代码正确但缺直接测试）。
func TestParseSSE_ToolCallsMissingIndexErrors(t *testing.T) {
	// call_1 占 index 0；call_2（不同 id）也发 index 0 → 同 index 归并不同 id，应报错。
	const sse = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"shell","arguments":"{}"}}]}}]}` + "\n" +
		"data: [DONE]\n"
	_, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("两个不同 id 归并到同 index 应报错（防漏 index 致多 tool_call 错误合并）")
	}
	if !strings.Contains(err.Error(), "index") {
		t.Errorf("错误应提及 index 归并，got %v", err)
	}

	// 对照：同 id 的多分片（index 0，首片带 id + 后续无 id 仅 arguments 片段）正常累积，不报错。
	const ok = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}}]}}]}` + "\n" +
		"data: [DONE]\n"
	resp, err := parseSSE(strings.NewReader(ok), nil)
	if err != nil {
		t.Fatalf("同 id 多分片应正常累积：%v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" {
		t.Errorf("单 tool_call 累积：got %+v", resp.ToolCalls)
	}
}
