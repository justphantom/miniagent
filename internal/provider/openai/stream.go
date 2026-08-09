package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/config"
)

// StreamClient calls the OpenAI-compatible chat completions endpoint (streaming SSE).
// The *http.Client for streaming has no overall Timeout (a total timeout covering body reads would truncate long
// generations, P2-5); total duration is controlled by ctx.
// Non-streaming goes through ChatClient (P4 split); SSE parsing is in stream_parse.go.
type StreamClient struct {
	APIKey                  string
	ChatURL                 string
	Headers                 map[string]string // custom request headers, does not override Authorization / Content-Type
	StreamAllowUnterminated bool              // opt-in: accept content-then-EOF (connection drop, no [DONE]/finish_reason) for non-compliant endpoints (vLLM/Ollama)
	chatURL                 *url.URL
	chatOnce                sync.Once
	chatErr                 error
	HTTP                    *http.Client
	Logger                  *slog.Logger
	defaultClient           *http.Client // cached streaming client (no Timeout) when c.HTTP==nil
	defaultClientOnce       sync.Once
	injectedClient          *http.Client // when c.HTTP has a total Timeout, a new no-Timeout client borrowing its Transport (cached)
	injectedClientOnce      sync.Once
}

// NewStreamClient parses and caches chatURL at construction time. headers are provider custom request headers, may be nil.
func NewStreamClient(apiKey, chatURL string, httpClient *http.Client, logger *slog.Logger, headers map[string]string) (*StreamClient, error) {
	chat, err := config.ValidateURL(chatURL)
	if err != nil {
		return nil, err
	}
	return &StreamClient{APIKey: apiKey, ChatURL: chatURL, chatURL: chat, HTTP: httpClient, Logger: logger, Headers: headers}, nil
}

// chatEndpoint returns the cached chatURL (lazy-parse fallback constructs it directly; sync.Once guarantees concurrency safety).
func (c *StreamClient) chatEndpoint() (*url.URL, error) {
	c.chatOnce.Do(func() { cacheEndpoint(&c.chatURL, &c.chatErr, c.ChatURL) })
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	return c.chatURL, nil
}

// streamClient returns the streaming http.Client: if the injected one has a total Timeout it would truncate the body
// (P2-5/P1-A), so build a new no-Timeout client borrowing its Transport (preserving the proxy, #2); if not injected, lazily
// construct and cache one.
func (c *StreamClient) streamClient() *http.Client {
	if c.HTTP != nil {
		if c.HTTP.Timeout > 0 {
			// An injected client with a total Timeout would truncate the body; borrow its Transport to build a new no-Timeout client
			// and cache it (symmetric with defaultClientOnce; previously rebuilding each time vs caching was asymmetric — currently
			// DoStream calls this once at the top so the cost is negligible, caching removes the asymmetry).
			c.injectedClientOnce.Do(func() { c.injectedClient = &http.Client{Transport: c.HTTP.Transport} })
			return c.injectedClient
		}
		return c.HTTP
	}
	c.defaultClientOnce.Do(func() { c.defaultClient = &http.Client{} })
	return c.defaultClient
}

// DoStream calls POST /v1/chat/completions (stream=true); onDelta pushes increments in real time and returns the aggregated Response.
// When onDelta returns an error, the stream is aborted immediately and that error is returned. Retry: in the pre-delta phase
// (client.Do failure or non-200, no delta streamed yet) reuse shouldRetryStatus + backoff + Retry-After; once parseSSE is
// entered (200, delta already streamed) it is irrevocable and not retried (P2-4).
func (c *StreamClient) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	if c.APIKey == "" {
		return miniagent.Response{}, errors.New("miniagent: api_key is empty")
	}
	u, err := c.chatEndpoint()
	if err != nil {
		return miniagent.Response{}, err
	}
	req.Stream = true
	body, err := buildChatBody(req)
	if err != nil {
		return miniagent.Response{}, fmt.Errorf("build request body: %w", err)
	}
	client := c.streamClient()
	if c.Logger != nil {
		c.Logger.Debug("llm stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	// deltaSent counts deltas pushed to onDelta during the current attempt. A parseSSE failure with deltaSent==0 means
	// nothing reached the caller yet (early reset / 200-then-EOF), so the call can be transparently retried without
	// duplicating live output; deltaSent>0 means the stream is irrevocable (live UX already received partial content).
	var deltaSent int
	wrappedOnDelta := func(d miniagent.Delta) error {
		deltaSent++
		if onDelta != nil {
			return onDelta(d)
		}
		return nil
	}
	backoff := retryBaseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return miniagent.Response{}, err
		}
		deltaSent = 0
		// Rebuild httpReq each iteration: the body reader from the previous round has been consumed; reusing it would send an empty body.
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return miniagent.Response{}, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		// Inject provider custom headers; skip Authorization/Content-Type to prevent overriding auth and content type
		// (aligned with the same-name loop in client.go / models.go — previously this loop omitted the skip, contradicting the field comment).
		for k, v := range c.Headers {
			if ck := http.CanonicalHeaderKey(k); ck == "Authorization" || ck == "Content-Type" {
				continue
			}
			httpReq.Header.Set(k, v)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			if c.Logger != nil {
				c.Logger.Warn("llm stream request failed", "error", err, "failed_attempt", attempt+1)
			}
			if attempt == maxRetries {
				return miniagent.Response{}, fmt.Errorf("after %d retries: llm request: %w", attempt, err)
			}
			if waitErr := sleepCtx(ctx, capRetryDelay(backoff, -1)); waitErr != nil {
				return miniagent.Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
			_ = resp.Body.Close()
			// 400/413 context over limit / thinking: reuse the non-streaming judgment for Run to degrade; no retry. attempt>0 adds an "after N retries" prefix (P3 debugging).
			// §P1-C: status gate widened from only 400 to 400||413 (aligned with client.go).
			prefix := ""
			if attempt > 0 {
				prefix = fmt.Sprintf("after %d retries: ", attempt)
			}
			if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
				// raw is only used for feature identification, not echoed into the error — aligned with the non-streaming client.go:
				// a malicious/debugging proxy could echo Authorization in the error body; the error flows via emitRunError into NDJSON stdout
				// and the session jsonl, which would leak the key.
				return miniagent.Response{}, fmt.Errorf("%s%w (status %d)", prefix, miniagent.ErrContextLength, resp.StatusCode)
			}
			if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
				return miniagent.Response{}, fmt.Errorf("%s%w (status %d)", prefix, miniagent.ErrThinkingUnsupported, resp.StatusCode)
			}
			if !shouldRetryStatus(resp.StatusCode) || attempt == maxRetries {
				return miniagent.Response{}, errors.New(prefix + fmt.Sprintf("llm returned %d", resp.StatusCode))
			}
			if c.Logger != nil {
				c.Logger.Warn("llm stream non-200, retrying", "status", resp.StatusCode, "failed_attempt", attempt+1)
			}
			if waitErr := sleepCtx(ctx, capRetryDelay(backoff, parseRetryAfter(resp.Header))); waitErr != nil {
				return miniagent.Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		// HTTP 200: parseSSE streams deltas (defer Close inside the IIFE: when onDelta panics — recovered by callLLMOnce —
		// the body is still closed; a function-level defer would accumulate across retries, so it is inlined per iteration).
		res, perr := func() (miniagent.Response, error) {
			defer func() { _ = resp.Body.Close() }()
			return parseSSE(resp.Body, wrappedOnDelta)
		}()
		if perr == nil {
			return res, nil
		}
		// Opt-in relaxation: accept a content-then-EOF stream (connection drop, no [DONE]/finish_reason) for non-compliant
		// endpoints (vLLM/Ollama). parseSSE returns the partial Response alongside errStreamUnterminated, so the caller
		// still receives the content that was streamed before the drop.
		if c.StreamAllowUnterminated && errors.Is(perr, errStreamUnterminated) {
			if c.Logger != nil {
				c.Logger.Warn("stream ended without [DONE]/finish_reason; accepted partial response under stream_allow_unterminated")
			}
			return res, nil
		}
		// parseSSE failed. If ZERO deltas were emitted (early reset / 200-then-EOF — the common LB/proxy first-byte drop)
		// and the error is transient, retry transparently: mirrors the non-streaming client's network retry with zero delta
		// duplication. Once a delta has streamed the call is irrevocable (P2-4): live UX already received partial content,
		// so surface the error as-is rather than replaying a half-stream.
		if deltaSent == 0 && isTransientStreamError(perr) && attempt < maxRetries {
			if c.Logger != nil {
				c.Logger.Warn("llm stream ended pre-delta, retrying", "error", perr, "failed_attempt", attempt+1)
			}
			if waitErr := sleepCtx(ctx, capRetryDelay(backoff, -1)); waitErr != nil {
				return miniagent.Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		return miniagent.Response{}, perr
	}
	return miniagent.Response{}, errors.New("llm stream retry loop exited unexpectedly")
}

// isTransientStreamError reports whether a parseSSE failure (in the zero-delta phase) is worth one transparent retry —
// mirroring the non-streaming client's network-error retry. Covers connection drops/resets that surface as a wrapped
// "read sse:" scanner error or io.ErrUnexpectedEOF, plus the 200-then-EOF "stream ended without any choices" case
// (LB/proxy first-byte reset). ctx cancellation/deadline is excluded (the loop's ctx.Err() check handles it); a JSON
// parse error of an actually-received chunk is excluded (likely persistent, not a connection blip).
func isTransientStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := err.Error()
	return strings.HasPrefix(s, "read sse:") || strings.Contains(s, "stream ended without any choices")
}
