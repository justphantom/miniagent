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
	"sync"
	"time"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/httpretry"
	"github.com/justphantom/miniagent/text"
)

const maxChatBodyBytes = 4 << 20 // 4 MiB; exactly at the limit so it does not truncate, 1 byte over errors

// ChatClient calls an OpenAI-compatible chat completions endpoint (non-streaming) + the models list.
// Lazy parsing (the test path that constructs the struct directly) is guarded by sync.Once to ensure
// concurrent Do calls are race-free (fixes R4). Streaming goes through StreamClient (P4 split); this
// client's *http.Client carries an overall Timeout as a fallback against a single call hanging.
type ChatClient struct {
	APIKey     string
	ChatURL    string
	ModelsURL  string
	Headers    map[string]string // custom request headers; does not override Authorization / Content-Type
	chatURL    *url.URL
	chatOnce   sync.Once
	chatErr    error
	modelsURL  *url.URL
	modelsOnce sync.Once
	modelsErr  error
	HTTP       *http.Client
	Logger     *slog.Logger
}

// NewChatClient parses and caches chatURL/modelsURL at construction time (review v3 #10). modelsURL
// may be empty (ListAllModels does not GET when falling back to the static list). headers is the
// provider's custom request header map and may be nil.
func NewChatClient(apiKey, chatURL, modelsURL string, httpClient *http.Client, logger *slog.Logger, headers map[string]string) (*ChatClient, error) {
	chat, err := text.ValidateURL(chatURL)
	if err != nil {
		return nil, err
	}
	c := &ChatClient{APIKey: apiKey, ChatURL: chatURL, ModelsURL: modelsURL, chatURL: chat, HTTP: httpClient, Logger: logger, Headers: headers}
	if modelsURL != "" {
		m, err := text.ValidateURL(modelsURL)
		if err != nil {
			return nil, err
		}
		c.modelsURL = m
	}
	return c, nil
}

// cacheEndpoint parses raw and caches it into dst (no-op if already set); on failure it caches the
// error into errp. The caller invokes it inside sync.Once.Do for concurrency safety (reused by
// chat/models, fixes R4).
func cacheEndpoint(dst **url.URL, errp *error, raw string) {
	if *dst != nil {
		return
	}
	u, err := text.ValidateURL(raw)
	if err != nil {
		*errp = err
		return
	}
	*dst = u
}

// chatEndpoint returns the cached chatURL (lazy-parse fallback for direct construction; sync.Once
// guarantees concurrency safety).
func (c *ChatClient) chatEndpoint(defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	c.chatOnce.Do(func() { cacheEndpoint(&c.chatURL, &c.chatErr, c.ChatURL) })
	if c.chatErr != nil {
		return nil, nil, c.chatErr
	}
	return c.client(defaultTimeout), c.chatURL, nil
}

func (c *ChatClient) modelsEndpoint(defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	c.modelsOnce.Do(func() {
		if c.modelsURL == nil && c.ModelsURL == "" {
			c.modelsErr = errors.New("miniagent: models_url not configured")
			return
		}
		cacheEndpoint(&c.modelsURL, &c.modelsErr, c.ModelsURL)
	})
	if c.modelsErr != nil {
		return nil, nil, c.modelsErr
	}
	return c.client(defaultTimeout), c.modelsURL, nil
}

// client returns the non-streaming http.Client. When c.HTTP != nil the injected client is reused;
// otherwise a new client is built per call with defaultTimeout (cheap to build, shares
// DefaultTransport so the connection pool is unaffected). The former defaultClient cache was shared
// by the chat(120s)/models(30s) endpoints, and sync.Once pinned the first caller's timeout while
// later defaultTimeout values were silently ignored, so the cache was removed.
func (c *ChatClient) client(defaultTimeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// Do posts to ChatURL (non-streaming) and parses choices[0] / usage / finish_reason. The response
// body is capped at maxChatBodyBytes; exceeding it errors.
//
// Retry policy: 429/500/502/503/504 and network errors are retried automatically up to maxRetries
// times with backoff of retryBaseDelay * 2^attempt; if the response carries Retry-After (seconds or
// HTTP-date) it replaces the backoff (still capped by retryMaxDelay). Other 4xx / parse errors /
// oversized bodies return immediately. Retries are cancelable via ctx.
func (c *ChatClient) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	client, u, body, err := c.prepareDo(req)
	if err != nil {
		return miniagent.Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("llm request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	backoff := httpretry.RetryBaseDelay
	for attempt := 0; attempt <= httpretry.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return miniagent.Response{}, err
		}
		resp, retryable, retryAfter, err := c.doOnce(ctx, client, u, body)
		if err == nil {
			return resp, nil
		}
		if !retryable || attempt == httpretry.MaxRetries {
			// Retries exhausted or non-retryable error: add "after N retries" context to aid debugging.
			if attempt > 0 {
				return miniagent.Response{}, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return miniagent.Response{}, err
		}
		delay := httpretry.CapRetryDelay(backoff, retryAfter)
		if c.Logger != nil {
			c.Logger.Warn("llm call failed, retrying", "failed_attempt", attempt+1, "delay_ms", delay.Milliseconds(), "error", err)
		}
		if waitErr := httpretry.SleepCtx(ctx, delay); waitErr != nil {
			return miniagent.Response{}, waitErr
		}
		backoff *= 2
	}
	// Normal loop exit (unreachable; all paths return inside the loop); kept as a defensive fallback.
	return miniagent.Response{}, errors.New("llm retry loop exited unexpectedly")
}

// doOnce performs a single HTTP call and parses it. retryable reports whether the error is worth
// retrying; retryAfter is the wait duration parsed from the Retry-After header (-1 means absent).
func (c *ChatClient) doOnce(ctx context.Context, client *http.Client, u *url.URL, body []byte) (miniagent.Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return miniagent.Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	// Redirect safety: relies on the standard library's default CheckRedirect — it strips
	// Authorization and other sensitive headers before cross-origin redirects. If a custom
	// CheckRedirect is added in the future, this semantics must be preserved.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", miniagent.UserAgent())
	// Inject provider custom headers; skip Authorization/Content-Type to avoid clobbering auth and
	// content-type (matches the comment promise above). User-Agent stays overridable: some gateways
	// require a specific UA string to pass.
	for k, v := range c.Headers {
		if ck := http.CanonicalHeaderKey(k); ck == "Authorization" || ck == "Content-Type" {
			continue
		}
		httpReq.Header.Set(k, v)
	}

	callStart := time.Now()
	resp, err := client.Do(httpReq)
	callDur := time.Since(callStart)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm request failed", "error", err, "duration_ms", callDur.Milliseconds())
		}
		// Network-layer error (connection refused / DNS / timeout): retryable, no Retry-After.
		return miniagent.Response{}, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
	if rerr != nil {
		return miniagent.Response{}, true, -1, fmt.Errorf("read response: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		// context-length-exceeded (400/413 + signature words) is singled out: the upper-layer Run
		// uses it to perform a single history-tightening retry. §P1-C: the status gate was widened
		// from 400-only to 400||413 (Anthropic request_too_large comes back as 413).
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
			// raw is used only for signature detection and is not echoed into the error — a malicious
			// or debug proxy may echo the request URL/Authorization in the error body, and the error
			// flows through emitRunError into NDJSON stdout and the session jsonl, leaking the key.
			// Only the status code + sentinel error type are surfaced.
			return miniagent.Response{}, false, 0, fmt.Errorf("%w (status %d)", miniagent.ErrContextLength, resp.StatusCode)
		}
		// thinking parameter unsupported (400 + signature words): callLLMWithDowngrade uses this to
		// retry once with the field dropped (review v2 #7).
		if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w (status %d)", miniagent.ErrThinkingUnsupported, resp.StatusCode)
		}
		msg := fmt.Sprintf("llm returned %d", resp.StatusCode)
		if shouldRetryStatus(resp.StatusCode) {
			return miniagent.Response{}, true, httpretry.ParseRetryAfter(resp.Header), errors.New(msg)
		}
		return miniagent.Response{}, false, 0, errors.New(msg)
	}
	// The 200 path only hard-fails on oversize: an oversize non-200 error body does not suppress
	// retries (status flows through shouldRetryStatus, matching stream.go/models.go).
	if int64(len(raw)) > maxChatBodyBytes {
		return miniagent.Response{}, false, 0, fmt.Errorf("response exceeded %d bytes", maxChatBodyBytes)
	}
	out, perr := parseChatResponse(raw)
	if perr != nil {
		return miniagent.Response{}, false, 0, perr
	}
	if c.Logger != nil {
		c.Logger.Info("llm call done", "duration_ms", callDur.Milliseconds(), "input_tokens", out.Usage.InputTokens, "output_tokens", out.Usage.OutputTokens, "tool_calls", len(out.ToolCalls), "finish_reason", out.FinishReason)
	}
	return out, false, 0, nil
}

func (c *ChatClient) prepareDo(req miniagent.Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	client, u, err := c.chatEndpoint(120 * time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Stream = false // Do is always non-streaming; guard against caller missetting it
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
}

// Provider is the default implementation of an OpenAI-compatible LLM: it composes ChatClient (Do,
// non-streaming) and StreamClient (DoStream, streaming) to satisfy the miniagent.LLM interface. The
// cmd wires it up to feed Run; a custom provider need only implement miniagent.LLM to replace it
// with zero changes to the core Run (this is the default impl of "provider as a plugin"). Stream is
// invoked only when cfg.Stream=true and may be nil in non-streaming scenarios.
type Provider struct {
	Chat   *ChatClient
	Stream *StreamClient
}

func (p *Provider) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	return p.Chat.Do(ctx, req)
}

func (p *Provider) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	return p.Stream.DoStream(ctx, req, onDelta)
}
