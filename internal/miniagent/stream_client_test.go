package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
