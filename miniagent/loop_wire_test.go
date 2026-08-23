package miniagent

import (
	"fmt"
	"strings"
)

func textResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

func toolResponse(calls ...ToolCall) string {
	tcs := make([]string, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}
