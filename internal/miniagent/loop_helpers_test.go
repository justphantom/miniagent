package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/text"
)

// This file centrally holds the LLM stubs and helpers for the core white-box tests
// of the loop (fakeTransport/recordingTransport/fakeLLM + wire replicas + response
// builders + lastToolMessage). These symbols are reused across multiple core _test
// files (package miniagent).
//
// Note: exported versions of the same mocks live in the internal/miniagent/looptest
// subpackage for reuse by external test packages (e.g. policy_test) — but core
// white-box tests cannot import looptest (looptest depends on miniagent, which would
// form a cycle), so the core keeps its own copy.

// fakeTransport replays preset non-streaming JSON bodies in call order. lastBody
// records the most recent request body, bodies records all of them for multi-step
// assertions, and calls accumulates the number of invocations.
type fakeTransport struct {
	responses []string
	statuses  []int
	calls     int
	lastBody  string
	bodies    []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.lastBody = string(b)
		f.bodies = append(f.bodies, string(b))
		_ = req.Body.Close()
	}
	idx := f.calls
	f.calls++
	status := http.StatusOK
	if idx < len(f.statuses) {
		status = f.statuses[idx]
	}
	body := ""
	if idx < len(f.responses) {
		body = f.responses[idx]
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// recordingTransport replays preset (status, body) pairs in order and records each request body.
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

// fakeLLM is a miniagent.LLM stub for core tests: it goes over HTTP via
// fakeTransport and carries its own OpenAI-compatible wire construction / parsing /
// retry (a test subset copied from the openai package logic), so core loop tests do
// not depend on the openai package (avoiding the core_test -> openai -> core test
// cycle). Used only by _test.go; never enters the production binary.
type fakeLLM struct {
	tr http.RoundTripper
}

// testClients builds a fakeLLM (name kept for historical reasons). Callers pass the
// returned LLM directly to Run.
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

// DoStream: core loop tests are all non-streaming (Run(Stream:false) only calls Do);
// kept for interface completeness, falls back to Do.
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

// ---- Below are test-only replicas of the openai package wire / retry logic (_test.go
// only, to avoid the core -> openai test cycle) ----

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

// testBuildChatBody replicates openai.testBuildChatBody: builds an OpenAI-compatible wire body.
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

// testIsThinkingError replicates openai.isThinkingError (tightened version: strong-signal field names + weak-signal thinking&unknown combination).
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

// textResponse builds non-streaming chat completions JSON: a single choice, plain-text reply, fixed usage {1,1}.
func textResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// toolResponse builds non-streaming chat completions JSON: a single choice with tool_calls (content always empty).
func toolResponse(calls ...ToolCall) string {
	tcs := make([]string, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, c.ID, c.Name, c.Args))
	}
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Join(tcs, ","))
}

// lastToolMessage returns the last role=tool message in msgs (test helper).
func lastToolMessage(t *testing.T, msgs []Message) Message {
	t.Helper()
	for idx := range slices.Backward(msgs) {
		if msgs[idx].Role == RoleTool {
			return msgs[idx]
		}
	}
	t.Fatalf("no tool message in msgs: %+v", msgs)
	return Message{}
}
