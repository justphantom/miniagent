package looptest

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

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

// FakeTransport replays preset non-streaming JSON bodies in call order. LastBody records the last request body;
// Bodies records all of them for multi-step assertions; Calls accumulates call count. Fields exported so tests can assert/construct directly.
type FakeTransport struct {
	Responses []string
	Statuses  []int
	Calls     int
	LastBody  string
	Bodies    []string
}

func (f *FakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.LastBody = string(b)
		f.Bodies = append(f.Bodies, string(b))
		_ = req.Body.Close()
	}
	idx := f.Calls
	f.Calls++
	status := http.StatusOK
	if idx < len(f.Statuses) {
		status = f.Statuses[idx]
	}
	body := ""
	if idx < len(f.Responses) {
		body = f.Responses[idx]
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// RecordingTransport replays preset (Status, Body) entries in Plan order, recording each request body.
type RecordingTransport struct {
	Plan   []TransportResp
	Bodies []string
	Calls  int
}

// TransportResp is a single playback entry for RecordingTransport.
type TransportResp struct {
	Status int
	Body   string
}

func (r *RecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.Bodies = append(r.Bodies, string(b))
		_ = req.Body.Close()
	}
	idx := r.Calls
	r.Calls++
	resp := TransportResp{Status: http.StatusOK, Body: ""}
	if idx < len(r.Plan) {
		resp = r.Plan[idx]
	}
	return &http.Response{
		StatusCode: resp.Status,
		Body:       io.NopCloser(strings.NewReader(resp.Body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// FakeLLM is a miniagent.LLM stub: it goes through a RoundTripper over HTTP, with built-in OpenAI-compatible
// wire construction/parsing/retry (a test copy of the openai package's logic), so loop tests do not depend
// on the openai package. Implements miniagent.LLM (Do + DoStream).
type FakeLLM struct {
	tr http.RoundTripper
}

// NewFakeLLM constructs a FakeLLM from tr (naming follows the historical testClients).
func NewFakeLLM(tr http.RoundTripper) *FakeLLM {
	return &FakeLLM{tr: tr}
}

func (f *FakeLLM) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	req.Stream = false
	body, err := BuildChatBody(req)
	if err != nil {
		return miniagent.Response{}, err
	}
	backoff := RetryBaseDelay
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return miniagent.Response{}, err
		}
		resp, retryable, retryAfter, err := f.doOnce(ctx, body)
		if err == nil {
			return resp, nil
		}
		if !retryable || attempt == MaxRetries {
			if attempt > 0 {
				return miniagent.Response{}, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return miniagent.Response{}, err
		}
		SleepCtx(ctx, CapRetryDelay(backoff, retryAfter))
		backoff *= 2
	}
	return miniagent.Response{}, errors.New("fake llm retry loop exited unexpectedly")
}

// DoStream: loop tests are all non-streaming (Run Stream:false only calls Do); kept for interface
// completeness, falls back to Do.
func (f *FakeLLM) DoStream(ctx context.Context, req miniagent.Request, _ func(miniagent.Delta) error) (miniagent.Response, error) {
	req.Stream = false
	return f.Do(ctx, req)
}

func (f *FakeLLM) doOnce(ctx context.Context, body []byte) (miniagent.Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost", bytes.NewReader(body))
	if err != nil {
		return miniagent.Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := f.tr.RoundTrip(httpReq)
	if err != nil {
		return miniagent.Response{}, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxChatBodyBytes+1))
	if rerr != nil {
		return miniagent.Response{}, true, -1, fmt.Errorf("read response: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w: %s", miniagent.ErrContextLength, text.Truncate(string(raw), 500, "…"))
		}
		if resp.StatusCode == http.StatusBadRequest && IsThinkingError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w: %s", miniagent.ErrThinkingUnsupported, text.Truncate(string(raw), 500, "…"))
		}
		msg := fmt.Sprintf("llm returned %d: %s", resp.StatusCode, text.Truncate(string(raw), 500, "…"))
		if ShouldRetryStatus(resp.StatusCode) {
			return miniagent.Response{}, true, ParseRetryAfter(resp.Header), errors.New(msg)
		}
		return miniagent.Response{}, false, 0, errors.New(msg)
	}
	out, perr := ParseChatResponse(raw)
	return out, false, 0, perr
}

// ---- Test-side copies of the openai package's wire / retry logic (kept in sync with the openai package implementation)----

const (
	MaxRetries       = 2
	RetryBaseDelay   = 500 * time.Millisecond
	RetryMaxDelay    = 8 * time.Second
	MaxChatBodyBytes = 4 << 20
)

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Fn   struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

// BuildChatBody replicates openai.testBuildChatBody: builds an OpenAI-compatible wire body so that the
// LastBody/Bodies recorded by FakeTransport match the real ChatClient (including "role":"system" / reasoning_effort).
func BuildChatBody(req miniagent.Request) ([]byte, error) {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: miniagent.RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, Content: m.Content, ReasoningContent: m.Reasoning, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			ctc := chatToolCall{ID: tc.ID, Type: "function"}
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
	if req.ThinkingLevel != "" && req.ThinkingLevel != miniagent.ThinkingOff && req.Thinking != nil {
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

// ParseChatResponse replicates openai.testParseChatResponse.
func ParseChatResponse(raw []byte) (miniagent.Response, error) {
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
		return miniagent.Response{}, fmt.Errorf("parse response: %w", err)
	}
	if len(v.Choices) == 0 {
		return miniagent.Response{}, errors.New("llm response has no choices")
	}
	ch := v.Choices[0]
	out := miniagent.Response{Text: ch.Message.Content, Reasoning: ch.Message.ReasoningContent, FinishReason: ch.FinishReason}
	if out.Reasoning == "" {
		out.Reasoning = ch.Message.Reasoning
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	if v.Usage != nil {
		out.Usage = miniagent.Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}

// ShouldRetryStatus replicates openai.shouldRetryStatus.
func ShouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// IsThinkingError replicates openai.isThinkingError (tightened version: strong-signal field names + the weak-signal thinking&unknown combination).
func IsThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning_effort_level") {
		return true
	}
	hasThinking := strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking")
	hasUnknown := strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unexpected argument")
	return hasThinking && hasUnknown
}

// ParseRetryAfter replicates openai.parseRetryAfter.
func ParseRetryAfter(h http.Header) time.Duration {
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

// CapRetryDelay replicates openai.capRetryDelay.
func CapRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > RetryMaxDelay {
		backoff = RetryMaxDelay
	}
	return backoff
}

// SleepCtx replicates openai.sleepCtx (respects ctx cancellation).
func SleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
