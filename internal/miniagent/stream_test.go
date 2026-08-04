package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 纯文本 + reasoning + usage + [DONE]：聚合正确，onDelta 收到每个片段。
func TestParseSSE_TextReasoningUsage(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hello "}}]}
data: {"choices":[{"delta":{"content":"world","reasoning_content":"think"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: {"usage":{"prompt_tokens":5,"completion_tokens":2}}
data: [DONE]
`
	var deltas []Delta
	res, err := parseSSE(strings.NewReader(sse), func(d Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if res.Text != "Hello world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Reasoning != "think" {
		t.Errorf("Reasoning = %q", res.Reasoning)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
	if res.Usage.InputTokens != 5 || res.Usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	// "Hello "、"world"、"think" 各一片。
	if len(deltas) != 3 {
		t.Errorf("deltas = %d (%+v)", len(deltas), deltas)
	}
}

// tool_calls 跨多 chunk 按 index 累积，多个 index 按升序聚合。
func TestParseSSE_ToolCallsAccumulated(t *testing.T) {
	const sse = "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"shell","arguments":"{}"}}]}}]}` + "\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n" +
		"data: [DONE]\n"
	res, err := parseSSE(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].ID != "call_1" || res.ToolCalls[0].Name != "read" || res.ToolCalls[0].Args != `{"path":"a.go"}` {
		t.Errorf("call0 = %+v", res.ToolCalls[0])
	}
	if res.ToolCalls[1].ID != "call_2" || res.ToolCalls[1].Name != "shell" || res.ToolCalls[1].Args != "{}" {
		t.Errorf("call1 = %+v", res.ToolCalls[1])
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
}

// DoStream 经 httptest 喂 SSE：聚合 Response + onDelta 推增量。
func TestDoStream_Aggregates(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: {"usage":{"prompt_tokens":3,"completion_tokens":1}}
data: [DONE]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	var deltas []Delta
	resp, err := llm.DoStream(context.Background(), Request{Model: "m"}, func(d Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "Hi" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if len(deltas) != 1 || deltas[0].Kind != DeltaText || deltas[0].Text != "Hi" {
		t.Errorf("deltas = %+v", deltas)
	}
}

// DoStream 非 200：pre-delta 阶段重试 maxRetries 次后仍失败上抛（含 "503"）。
// Retry-After: 0 使退避即时，避免测试等待（重试耗尽路径的严格断言见 client_retry_test.go）。
func TestDoStream_NonOKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "busy")
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	_, err := llm.DoStream(context.Background(), Request{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want 503", err)
	}
}

// P3：DoStream 在 attempt>0 收到 context-length 400 时，错误带 "after N retries" 前缀（与
// 通用非 200 路径一致，排错信息）；哨兵 ErrContextLength 链仍可被 errors.Is 命中供 Run 降级。
// 流程：首次 503 触发一次重试 → 第二次返 400 context-length（在 attempt=1，即 after 1 retries）。
func TestDoStream_ContextLengthAfterRetry(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "busy")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens."}}`)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL}
	_, err := llm.DoStream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrContextLength) {
		t.Errorf("err = %v, want ErrContextLength in chain", err)
	}
	if !strings.Contains(err.Error(), "after 1 retries") {
		t.Errorf("err = %q, want \"after 1 retries\" prefix", err.Error())
	}
}

// P1-2：流直接以 [DONE] 开始/无 choices/仅 usage，必须报错而非返回空 Response 伪装成功。
func TestParseSSE_EmptyStreamErrors(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{"done-only", "data: [DONE]\n"},
		{"empty-input", ""},
		{"usage-only-no-choices", `data: {"usage":{"prompt_tokens":5,"completion_tokens":0}}` + "\ndata: [DONE]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSSE(strings.NewReader(tc.sse), nil)
			if err == nil {
				t.Fatalf("expected error for %s stream", tc.name)
			}
			if !strings.Contains(err.Error(), "without any choices") {
				t.Errorf("%s: err = %v", tc.name, err)
			}
		})
	}
}

// P1-3：provider 中途以 {"error":{"message":...}} chunk 报错，必须上抛而非吞掉当成功。
func TestParseSSE_MidStreamError(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n" +
		`data: {"error":{"message":"content filter triggered"}}` + "\n" +
		"data: [DONE]\n"
	_, err := parseSSE(strings.NewReader(sse), nil)
	if err == nil {
		t.Fatal("expected error for mid-stream error chunk")
	}
	if !strings.Contains(err.Error(), "content filter triggered") {
		t.Errorf("err should carry provider message: %v", err)
	}
	if !strings.Contains(err.Error(), "stream error from provider") {
		t.Errorf("err should be flagged as provider stream error: %v", err)
	}
}

// P3-2：单个 data 行载荷 > 1MB（旧上限）但 < 4MB（新上限）应能解析，不触发 ErrTooLong。
func TestParseSSE_LongLine(t *testing.T) {
	big := strings.Repeat("a", 1500*1024)
	chunk := fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, big)
	sse := "data: " + chunk + "\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n" +
		"data: [DONE]\n"
	res, err := parseSSE(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("parseSSE long line: %v", err)
	}
	if len(res.Text) != len(big) {
		t.Errorf("Text len = %d, want %d", len(res.Text), len(big))
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
}

// P2-5/P1-A：c.HTTP==nil 时 streamClient 返回无 Timeout 的 client（流式总时长由 ctx 控制，
// http.Client.Timeout 覆盖 body 读取会砍断长流）；缓存同一实例；注入时沿用注入。
// 注入契约（buildLLM 注入带 120s Timeout 的 client）：streamClient 须把注入 client 的
// 总 Timeout 清零（P1-A：流式 body 不被砍），但保留其 Transport（代理/连接配置，#2）。
func TestStreamClient_StreamClientNoTimeout(t *testing.T) {
	c := &StreamClient{APIKey: "sk", ChatURL: "http://x"}
	sc := c.streamClient()
	if sc.Timeout != 0 {
		t.Errorf("stream client Timeout = %v, want 0", sc.Timeout)
	}
	if c.streamClient() != sc {
		t.Error("stream client not cached")
	}
	// 模拟新 buildLLM 注入：带 120s 总 Timeout + 自定义 Transport 的 client。
	inj := &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{}}
	c2 := &StreamClient{APIKey: "sk", ChatURL: "http://x", HTTP: inj}
	got := c2.streamClient()
	if got.Timeout != 0 {
		t.Errorf("stream client Timeout = %v, want 0（流式清零总 Timeout）", got.Timeout)
	}
	if got.Transport != inj.Transport {
		t.Error("stream client 须保留注入的 Transport（代理/连接配置）")
	}
}

// Run + Stream 端到端：cfg.Stream=true 走 DoStream，OnDelta 推增量，聚合 Text。
func TestRun_StreamAggregates(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"streamed"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`
	tr := &fakeTransport{responses: []string{sse}}
	chat, stream := testClients(tr)
	var deltas []string
	hooks := LoopHooks{OnDelta: func(step int, kind DeltaKind, text string) error {
		deltas = append(deltas, text)
		return nil
	}}
	res, err := Run(context.Background(), chat, stream, LoopConfig{Model: "m", Stream: true}, "hi", hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "streamed" {
		t.Errorf("Text = %q", res.Text)
	}
	if len(deltas) != 1 || deltas[0] != "streamed" {
		t.Errorf("deltas = %v", deltas)
	}
}

// parseSSE 遇到非法 JSON chunk 上报错误（中途断流/乱数据）。
func TestParseSSE_MalformedChunk(t *testing.T) {
	sse := "data: not-json\n\ndata: [DONE]\n"
	if _, err := parseSSE(strings.NewReader(sse), nil); err == nil {
		t.Error("malformed chunk should error")
	}
}

func TestEmitDelta(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitDelta(&buf, 2, DeltaText, "hi"); err != nil {
		t.Fatalf("EmitDelta: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "text_delta" || ev["step"] != float64(2) || ev["text"] != "hi" {
		t.Errorf("event = %+v", ev)
	}
	buf.Reset()
	if err := EmitDelta(&buf, 3, DeltaReasoning, "think"); err != nil {
		t.Fatalf("EmitDelta: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev["type"] != "reasoning_delta" {
		t.Errorf("event = %+v", ev)
	}
}

// DoStream 携带自定义请求头。
func TestDoStream_CustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "stream-val" {
			t.Errorf("X-Custom = %q, want stream-val", r.Header.Get("X-Custom"))
		}
		if r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`)
	}))
	defer srv.Close()
	llm := &StreamClient{APIKey: "sk", ChatURL: srv.URL, Headers: map[string]string{"X-Custom": "stream-val"}}
	var deltas []Delta
	resp, err := llm.DoStream(context.Background(), Request{Model: "m"}, func(d Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if resp.Text != "hi" {
		t.Errorf("Text = %q", resp.Text)
	}
	if len(deltas) != 1 || deltas[0].Text != "hi" {
		t.Errorf("deltas = %+v", deltas)
	}
}
