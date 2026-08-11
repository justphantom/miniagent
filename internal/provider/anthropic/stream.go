package anthropic

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

// StreamClient calls the Anthropic Messages API (streaming SSE). Mirrors openai.StreamClient; differs in
// auth headers, wire shape (buildBody), and SSE parsing (parseSSE — Anthropic has no [DONE] marker;
// message_stop terminates). The *http.Client for streaming has no overall Timeout (a total timeout covering
// body reads would truncate long generations); total duration is controlled by ctx.
type StreamClient struct {
	APIKey                  string
	ChatURL                 string
	Headers                 map[string]string
	Cache                   bool
	StreamAllowUnterminated bool // opt-in: accept content-then-EOF (no message_stop) for non-compliant endpoints
	chatURL                 *url.URL
	chatOnce                sync.Once
	chatErr                 error
	HTTP                    *http.Client
	Logger                  *slog.Logger
	defaultClient           *http.Client // cached streaming client (no Timeout) when c.HTTP==nil
	defaultClientOnce       sync.Once
	injectedClient          *http.Client // when c.HTTP has a total Timeout, a no-Timeout client borrowing its Transport (cached)
	injectedClientOnce      sync.Once
}

// NewStreamClient parses and caches chatURL at construction time. headers may be nil; cache toggles caching.
func NewStreamClient(apiKey, chatURL string, httpClient *http.Client, logger *slog.Logger, headers map[string]string, cache bool) (*StreamClient, error) {
	u, err := config.ValidateURL(chatURL)
	if err != nil {
		return nil, err
	}
	return &StreamClient{APIKey: apiKey, ChatURL: chatURL, chatURL: u, HTTP: httpClient, Logger: logger, Headers: headers, Cache: cache}, nil
}

func (c *StreamClient) chatEndpoint() (*url.URL, error) {
	c.chatOnce.Do(func() {
		if c.chatURL == nil {
			u, err := config.ValidateURL(c.ChatURL)
			if err != nil {
				c.chatErr = err
				return
			}
			c.chatURL = u
		}
	})
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	return c.chatURL, nil
}

// streamClient returns the streaming http.Client: if the injected one has a total Timeout it would truncate
// the body, so build a new no-Timeout client borrowing its Transport (preserving the proxy); otherwise reuse
// the injected client, or lazily construct and cache one. Mirrors openai.StreamClient.streamClient.
func (c *StreamClient) streamClient() *http.Client {
	if c.HTTP != nil {
		if c.HTTP.Timeout > 0 {
			c.injectedClientOnce.Do(func() { c.injectedClient = &http.Client{Transport: c.HTTP.Transport} })
			return c.injectedClient
		}
		return c.HTTP
	}
	c.defaultClientOnce.Do(func() { c.defaultClient = &http.Client{} })
	return c.defaultClient
}

func (c *StreamClient) setAuthHeaders(httpReq *http.Request) {
	httpReq.Header.Set("X-Api-Key", c.APIKey)
	httpReq.Header.Set("Anthropic-Version", "2023-06-01")
	httpReq.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.Headers {
		if ck := http.CanonicalHeaderKey(k); ck == "X-Api-Key" || ck == "Anthropic-Version" || ck == "Anthropic-Beta" || ck == "Content-Type" {
			continue
		}
		httpReq.Header.Set(k, v)
	}
}

// DoStream calls POST /v1/messages (stream=true); onDelta pushes increments in real time and returns the
// aggregated Response. When onDelta returns an error, the stream is aborted immediately. Retry: in the
// pre-delta phase (client.Do failure or non-200, no delta streamed yet) reuse shouldRetryStatus + backoff +
// Retry-After; once parseSSE is entered (200, delta already streamed) it is irrevocable and not retried.
func (c *StreamClient) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	if c.APIKey == "" {
		return miniagent.Response{}, errors.New("miniagent: api_key is empty")
	}
	u, err := c.chatEndpoint()
	if err != nil {
		return miniagent.Response{}, err
	}
	req.Stream = true
	body, err := buildBody(req, c.Cache)
	if err != nil {
		return miniagent.Response{}, fmt.Errorf("build request body: %w", err)
	}
	client := c.streamClient()
	if c.Logger != nil {
		c.Logger.Debug("llm stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	// deltaSent counts deltas pushed during the current attempt. A parseSSE failure with deltaSent==0 means
	// nothing reached the caller yet (early reset / 200-then-EOF), so the call can be transparently retried
	// without duplicating live output; deltaSent>0 means the stream is irrevocable.
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
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return miniagent.Response{}, fmt.Errorf("build request: %w", err)
		}
		c.setAuthHeaders(httpReq)
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
			prefix := ""
			if attempt > 0 {
				prefix = fmt.Sprintf("after %d retries: ", attempt)
			}
			if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
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
		res, perr := func() (miniagent.Response, error) {
			defer func() { _ = resp.Body.Close() }()
			return parseSSE(resp.Body, wrappedOnDelta)
		}()
		if perr == nil {
			return res, nil
		}
		if c.StreamAllowUnterminated && errors.Is(perr, errStreamUnterminated) {
			if c.Logger != nil {
				c.Logger.Warn("stream ended without message_stop; accepted partial response under stream_allow_unterminated")
			}
			return res, nil
		}
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

// isTransientStreamError mirrors openai: a parseSSE failure in the zero-delta phase worth one transparent
// retry — connection drops/resets surfacing as a wrapped "read sse:" scanner error or io.ErrUnexpectedEOF,
// plus the 200-then-EOF cases (no message_start / no content). ctx cancellation is excluded.
func isTransientStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := err.Error()
	return strings.HasPrefix(s, "read sse:") || strings.Contains(s, "without message_start") || strings.Contains(s, "without any content")
}
