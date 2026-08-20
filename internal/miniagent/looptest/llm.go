package looptest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/text"
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
		_ = SleepCtx(ctx, CapRetryDelay(backoff, retryAfter))
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
