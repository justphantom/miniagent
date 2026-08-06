package miniagent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBuildChatBody_ThinkingLevelWritten(t *testing.T) {
	body, err := testBuildChatBody(Request{Model: "m", ThinkingLevel: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning_effort":"medium"`) {
		t.Errorf("thinking level not written: %s", body)
	}
}

func TestBuildChatBody_ThinkingOffOmitted(t *testing.T) {
	for _, lvl := range []string{"", ThinkingOff} {
		body, _ := testBuildChatBody(Request{Model: "m", ThinkingLevel: lvl})
		if strings.Contains(string(body), "reasoning_effort") {
			t.Errorf("level %q should omit thinking: %s", lvl, body)
		}
	}
}

func TestBuildChatBody_ThinkingMappingOverrides(t *testing.T) {
	req := Request{
		Model:         "m",
		ThinkingLevel: "medium",
		Thinking:      &ThinkingMapping{Field: "effort", Map: map[string]string{"medium": "high"}},
	}
	body, _ := testBuildChatBody(req)
	if !strings.Contains(string(body), `"effort":"high"`) {
		t.Errorf("mapping override not applied: %s", body)
	}
	if strings.Contains(string(body), "reasoning_effort") {
		t.Errorf("default field should be replaced: %s", body)
	}
}

// recordingTransport 记录每次请求体，按序回放预设 (status, body)。
type recordingTransport struct {
	plan   []transportResp
	bodies []string
	calls  int
}
type transportResp struct {
	status int
	body   string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.bodies = append(r.bodies, string(b))
		_ = req.Body.Close()
	}
	idx := r.calls
	r.calls++
	resp := transportResp{status: http.StatusOK, body: ""}
	if idx < len(r.plan) {
		resp = r.plan[idx]
	}
	return &http.Response{
		StatusCode: resp.status,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// 400 含 thinking 特征 → callLLM 去 thinking 重试一次（审查 v2 #7）。
func TestCallLLM_Thinking400Downgrade(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
		{status: http.StatusOK, body: textResponse("ok")},
	}}
	llm := testClients(tr)
	resp, err := callLLM(context.Background(), llm, LoopConfig{Model: "m", ThinkingLevel: "medium"}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("callLLM: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	if tr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (downgrade retry)", tr.calls)
	}
	if !strings.Contains(tr.bodies[0], `"reasoning_effort":"medium"`) {
		t.Errorf("first body should carry thinking: %s", tr.bodies[0])
	}
	if strings.Contains(tr.bodies[1], "reasoning_effort") {
		t.Errorf("second body should drop thinking: %s", tr.bodies[1])
	}
}

// 非 thinking 的 400 不触发降级（无 thinking 发送时直接上抛）。
func TestCallLLM_Plain400NoDowngrade(t *testing.T) {
	tr := &recordingTransport{plan: []transportResp{
		{status: http.StatusBadRequest, body: `{"error":{"message":"unknown parameter: reasoning_effort"}}`},
	}}
	llm := testClients(tr)
	_, err := callLLM(context.Background(), llm, LoopConfig{Model: "m"}, 1, []Message{{Role: "user", Content: "q"}}, LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error for plain 400")
	}
	if tr.calls != 1 {
		t.Errorf("calls = %d, want 1 (no downgrade without thinking)", tr.calls)
	}
}
