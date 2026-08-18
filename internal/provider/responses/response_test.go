package responses

import (
	"strings"
	"testing"
)

func responseBody() string {
	return `{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"thought"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"x\"}"}],"usage":{"input_tokens":7,"output_tokens":11}}`
}

func TestParseResponse_ItemsUsageAndReasoningState(t *testing.T) {
	resp, err := parseResponse([]byte(responseBody()))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "answer" || resp.Reasoning != "thought" {
		t.Errorf("text/reasoning = %q/%q", resp.Text, resp.Reasoning)
	}
	if resp.ReasoningState != `[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"thought"}]}]` {
		t.Errorf("ReasoningState = %s", resp.ReasoningState)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" || resp.ToolCalls[0].Name != "read" {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
	if resp.FinishReason != "tool_calls" || resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 11 {
		t.Errorf("finish/usage = %q/%+v", resp.FinishReason, resp.Usage)
	}
}

func TestParseResponse_IncompleteMapsMaxLength(t *testing.T) {
	resp, err := parseResponse([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "length" || resp.Text != "" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestParseResponse_FailedAndUnsupported(t *testing.T) {
	if _, err := parseResponse([]byte(`{"status":"failed","error":{"message":"boom","code":"server_error"}}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want provider failure", err)
	}
	if _, err := parseResponse([]byte(`{"status":"completed","output":[{"type":"web_search_call"}]}`)); err == nil || !strings.Contains(err.Error(), "web_search_call") {
		t.Fatalf("err = %v, want unsupported output item", err)
	}
}
