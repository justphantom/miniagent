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
	"sync"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/provider/httpretry"
)

const maxChatBodyBytes = 4 << 20 // 4 MiB; mirrors openai — exactly at the limit so it does not truncate

// Client calls the Anthropic Messages API (non-streaming). Mirrors openai.ChatClient structure; differs in
// auth headers (x-api-key + anthropic-version + interleaved-thinking beta), wire shape (buildBody), and
// response parsing (parseResponse). The *http.Client carries an overall Timeout as a fallback against a
// single call hanging; streaming goes through StreamClient.
type Client struct {
	APIKey     string
	ChatURL    string
	ModelsURL  string
	Headers    map[string]string
	Cache      bool
	chatURL    *url.URL
	chatOnce   sync.Once
	chatErr    error
	modelsURL  *url.URL
	modelsOnce sync.Once
	modelsErr  error
	HTTP       *http.Client
	Logger     *slog.Logger
}

// NewClient parses and caches chatURL/modelsURL at construction time (empty modelsURL = static models
// list, no endpoint). headers is the provider's custom request header map (may be nil); cache toggles
// prompt-caching breakpoints.
func NewClient(apiKey, chatURL, modelsURL string, httpClient *http.Client, logger *slog.Logger, headers map[string]string, cache bool) (*Client, error) {
	u, err := config.ValidateURL(chatURL)
	if err != nil {
		return nil, err
	}
	c := &Client{APIKey: apiKey, ChatURL: chatURL, ModelsURL: modelsURL, chatURL: u, HTTP: httpClient, Logger: logger, Headers: headers, Cache: cache}
	if modelsURL != "" {
		mu, err := config.ValidateURL(modelsURL)
		if err != nil {
			return nil, err
		}
		c.modelsURL = mu
	}
	return c, nil
}

// chatEndpoint returns the cached chatURL (lazy-parse fallback for direct construction; sync.Once for safety).
func (c *Client) chatEndpoint(defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
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
		return nil, nil, c.chatErr
	}
	return c.client(defaultTimeout), c.chatURL, nil
}

// modelsEndpoint mirrors chatEndpoint for ModelsURL. Empty ModelsURL = static models list only (config
// guarantees the empty case never reaches ListModels via ListAllModels).
func (c *Client) modelsEndpoint(defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	if c.ModelsURL == "" {
		return nil, nil, errors.New("models url is empty (static models list only)")
	}
	c.modelsOnce.Do(func() {
		if c.modelsURL == nil {
			u, err := config.ValidateURL(c.ModelsURL)
			if err != nil {
				c.modelsErr = err
				return
			}
			c.modelsURL = u
		}
	})
	if c.modelsErr != nil {
		return nil, nil, c.modelsErr
	}
	u := *c.modelsURL
	q := u.Query()
	q.Set("limit", "1000")
	u.RawQuery = q.Encode()
	return c.client(defaultTimeout), &u, nil
}

// client returns the non-streaming http.Client (injected one reused; otherwise built per call with timeout).
func (c *Client) client(defaultTimeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// setAuthHeaders sets the Anthropic auth headers (x-api-key + anthropic-version + interleaved-thinking beta
// + Content-Type), then injects provider custom headers, skipping the reserved keys so they cannot clobber
// auth. The interleaved-thinking beta is sent unconditionally: the API accepts it on every model and silently
// ignores it where unsupported, so it is always safe.
func (c *Client) setAuthHeaders(httpReq *http.Request) {
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

// Do posts to ChatURL (non-streaming) and parses the content blocks / stop_reason / usage. Retry policy
// mirrors openai.ChatClient.Do: 429/500/502/503/504 and network errors retry up to maxRetries with backoff
// (Retry-After replaces backoff, capped); context-length (400/413) and thinking-mismatch (400) map to their
// sentinel errors for the core's degradation paths; other 4xx return immediately.
func (c *Client) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
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
	return miniagent.Response{}, errors.New("llm retry loop exited unexpectedly")
}

// doOnce performs a single HTTP call and parses it. retryable reports whether the error is worth retrying;
// retryAfter is the wait parsed from Retry-After (-1 absent). raw is used only for signature detection and
// is never echoed into the error (a proxy may echo the request URL/key in the error body).
func (c *Client) doOnce(ctx context.Context, client *http.Client, u *url.URL, body []byte) (miniagent.Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return miniagent.Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	c.setAuthHeaders(httpReq)
	callStart := time.Now()
	resp, err := client.Do(httpReq)
	callDur := time.Since(callStart)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm request failed", "error", err, "duration_ms", callDur.Milliseconds())
		}
		return miniagent.Response{}, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
	if rerr != nil {
		return miniagent.Response{}, true, -1, fmt.Errorf("read response: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		// §P1-C: Anthropic request_too_large may come back as 413; gate on 400||413 like the openai provider.
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w (status %d)", miniagent.ErrContextLength, resp.StatusCode)
		}
		if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w (status %d)", miniagent.ErrThinkingUnsupported, resp.StatusCode)
		}
		msg := fmt.Sprintf("llm returned %d", resp.StatusCode)
		if shouldRetryStatus(resp.StatusCode) {
			return miniagent.Response{}, true, httpretry.ParseRetryAfter(resp.Header), errors.New(msg)
		}
		return miniagent.Response{}, false, 0, errors.New(msg)
	}
	if int64(len(raw)) > maxChatBodyBytes {
		return miniagent.Response{}, false, 0, fmt.Errorf("response exceeded %d bytes", maxChatBodyBytes)
	}
	out, perr := parseResponse(raw)
	if perr != nil {
		return miniagent.Response{}, false, 0, perr
	}
	if c.Logger != nil {
		c.Logger.Info("llm call done", "duration_ms", callDur.Milliseconds(), "input_tokens", out.Usage.InputTokens, "output_tokens", out.Usage.OutputTokens, "tool_calls", len(out.ToolCalls), "finish_reason", out.FinishReason)
	}
	return out, false, 0, nil
}

func (c *Client) prepareDo(req miniagent.Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	client, u, err := c.chatEndpoint(120 * time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Stream = false // Do is always non-streaming; guard against caller missetting it
	body, err := buildBody(req, c.Cache)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
}
