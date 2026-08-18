package responses

import (
	"errors"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestParseSSE_CompletedIsAuthoritative(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"par"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`data: {"type":"response.completed","response":` + responseBody() + `}`,
		"",
	}, "\n\n"))
	var deltas []miniagent.Delta
	resp, err := parseSSE(stream, func(d miniagent.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "answer" || resp.Reasoning != "thought" {
		t.Errorf("completed response = %+v", resp)
	}
	if len(deltas) != 2 || deltas[0].Kind != miniagent.DeltaText || deltas[1].Kind != miniagent.DeltaReasoning {
		t.Errorf("deltas = %+v", deltas)
	}
}

func TestParseSSE_FunctionDeltasBeforeCompleted(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"x\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"1}"}`,
		`data: {"type":"response.completed","response":` + responseBody() + `}`,
		"",
	}, "\n\n"))
	resp, err := parseSSE(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Args != `{"path":"x"}` {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
}

func TestParseSSE_UnterminatedAndProviderError(t *testing.T) {
	resp, err := parseSSE(strings.NewReader(strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"echo"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		"",
	}, "\n\n")), nil)
	if !errors.Is(err, errStreamUnterminated) {
		t.Fatalf("err = %v, want unterminated", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Args != "{}" {
		t.Errorf("partial ToolCalls = %+v", resp.ToolCalls)
	}
	_, err = parseSSE(strings.NewReader(`data: {"type":"error","error":{"message":"boom","code":"x"}}`+"\n\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want provider error", err)
	}
}
