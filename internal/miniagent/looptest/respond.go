// Package looptest provides the LLM test stub shared by the loop core (package miniagent) and the
// policy subpackage (package policy): it goes through FakeTransport over HTTP and ships its own
// OpenAI-compatible wire construction/parsing/retry (a test-subset copy of the openai package logic),
// so loop tests do not depend on the openai package — avoiding a core_test → openai → core test cycle.
// Referenced only by tests; it never enters the production binary (though not a _test.go file, its
// exported symbols are consumed only by _test packages in each subpackage).
package looptest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// TextResponse builds non-streaming chat completions JSON: a single choice with a plain-text reply and a fixed usage of {1,1}.
func TextResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// ToolResponse builds non-streaming chat completions JSON: a single choice carrying tool_calls (content is always empty).
func ToolResponse(calls ...miniagent.ToolCall) string {
	tcs := make([]string, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}

// LastToolMessage returns the last role=tool message in msgs (a test helper).
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
