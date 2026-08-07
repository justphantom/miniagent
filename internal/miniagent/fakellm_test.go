package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/text"
)

// fakeLLM 是 core 测试用的 miniagent.LLM 桩：经 fakeTransport 走 HTTP，自带 OpenAI 兼容
// wire 构造 / 解析 / 重试（复制 openai 包逻辑的测试子集），使 core 循环测试不依赖 openai 包
// （避免 core_test → openai → core 的测试环）。仅 _test.go 使用，不进生产二进制。
//
// 保留 fakeTransport 的 calls / lastBody / bodies 语义，故所有 loop 测试断言零改动：
// lastBody 仍是 wire 格式 JSON（含 "role":"system" / reasoning_effort），由 testBuildChatBody 构造。
type fakeLLM struct {
	tr http.RoundTripper
}

// testClients 构造 fakeLLM（命名沿用历史）。调用方直接把返回的 LLM 传给 Run。
func testClients(tr http.RoundTripper) *fakeLLM {
	return &fakeLLM{tr: tr}
}

func (f *fakeLLM) Do(ctx context.Context, req Request) (Response, error) {
	req.Stream = false
	body, err := testBuildChatBody(req)
	if err != nil {
		return Response{}, err
	}
	backoff := testRetryBaseDelay
	for attempt := 0; attempt <= testMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		resp, retryable, retryAfter, err := f.doOnce(ctx, body)
		if err == nil {
			return resp, nil
		}
		if !retryable || attempt == testMaxRetries {
			if attempt > 0 {
				return Response{}, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return Response{}, err
		}
		testSleepCtx(ctx, testCapRetryDelay(backoff, retryAfter))
		backoff *= 2
	}
	return Response{}, errors.New("fake llm retry loop exited unexpectedly")
}

// DoStream：core loop 测试均非流式（Run(Stream:false) 只调 Do）；保留接口完整性，fallback 到 Do。
func (f *fakeLLM) DoStream(ctx context.Context, req Request, _ func(Delta) error) (Response, error) {
	req.Stream = false
	return f.Do(ctx, req)
}

func (f *fakeLLM) doOnce(ctx context.Context, body []byte) (Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost", bytes.NewReader(body))
	if err != nil {
		return Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := f.tr.RoundTrip(httpReq)
	if err != nil {
		return Response{}, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, testMaxChatBodyBytes+1))
	if rerr != nil {
		return Response{}, true, -1, fmt.Errorf("read response: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && IsContextLengthError(raw) {
			return Response{}, false, 0, fmt.Errorf("%w: %s", ErrContextLength, text.Truncate(string(raw), 500, "…"))
		}
		if resp.StatusCode == http.StatusBadRequest && testIsThinkingError(raw) {
			return Response{}, false, 0, fmt.Errorf("%w: %s", ErrThinkingUnsupported, text.Truncate(string(raw), 500, "…"))
		}
		msg := fmt.Sprintf("llm returned %d: %s", resp.StatusCode, text.Truncate(string(raw), 500, "…"))
		if testShouldRetryStatus(resp.StatusCode) {
			return Response{}, true, testParseRetryAfter(resp.Header), errors.New(msg)
		}
		return Response{}, false, 0, errors.New(msg)
	}
	out, perr := testParseChatResponse(raw)
	return out, false, 0, perr
}

// ---- 以下为 openai 包 wire / 重试逻辑的测试用副本（仅 _test.go，避免 core→openai 测试环）----

const (
	testMaxRetries       = 2
	testRetryBaseDelay   = 500 * time.Millisecond
	testRetryMaxDelay    = 8 * time.Second
	testMaxChatBodyBytes = 4 << 20
)

type testChatMessage struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []testChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string             `json:"tool_call_id,omitempty"`
}

type testChatToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Fn   struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

// testBuildChatBody 复刻 openai.testBuildChatBody：构造 OpenAI 兼容 wire body，使 fakeTransport
// 记录的 lastBody / bodies 与真实 ChatClient 一致（含 "role":"system" / reasoning_effort）。
func testBuildChatBody(req Request) ([]byte, error) {
	msgs := make([]testChatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, testChatMessage{Role: RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		cm := testChatMessage{Role: m.Role, Content: m.Content, ReasoningContent: m.Reasoning, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			ctc := testChatToolCall{ID: tc.ID, Type: "function"}
			ctc.Fn.Name = tc.Name
			ctc.Fn.Args = tc.Args
			cm.ToolCalls = append(cm.ToolCalls, ctc)
		}
		msgs = append(msgs, cm)
	}
	payload := map[string]any{"model": req.Model, "messages": msgs}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.ThinkingLevel != "" && req.ThinkingLevel != ThinkingOff && req.Thinking != nil {
		field, val := req.Thinking.Field, req.ThinkingLevel
		if mapped, ok := req.Thinking.Map[req.ThinkingLevel]; ok {
			val = mapped
		}
		payload[field] = val
	}
	if req.Stream {
		payload["stream"] = true
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		funcs := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			funcs = append(funcs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		payload["tools"] = funcs
	}
	return json.Marshal(payload)
}

func testParseChatResponse(raw []byte) (Response, error) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return Response{}, fmt.Errorf("parse response: %w", err)
	}
	if len(v.Choices) == 0 {
		return Response{}, errors.New("llm response has no choices")
	}
	ch := v.Choices[0]
	out := Response{Text: ch.Message.Content, Reasoning: ch.Message.ReasoningContent, FinishReason: ch.FinishReason}
	if out.Reasoning == "" {
		out.Reasoning = ch.Message.Reasoning
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	if v.Usage != nil {
		out.Usage = Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}

func testShouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// testIsThinkingError 复刻 openai.isThinkingError（收紧版：强信号字段名 + 弱信号 thinking&unknown 组合）。
func testIsThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning_effort_level") {
		return true
	}
	hasThinking := strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking")
	hasUnknown := strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unexpected argument")
	return hasThinking && hasUnknown
}

func testParseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return -1
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return -1
}

func testCapRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > testRetryMaxDelay {
		backoff = testRetryMaxDelay
	}
	return backoff
}

func testSleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
