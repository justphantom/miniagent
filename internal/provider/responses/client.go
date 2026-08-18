package responses

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
	"github.com/justphantom/miniagent/internal/provider/httpretry"
	"github.com/justphantom/miniagent/internal/text"
)

const maxResponseBodyBytes = 4 << 20

type Client struct {
	APIKey       string
	Endpoint     string
	Headers      map[string]string
	HTTP         *http.Client
	Logger       *slog.Logger
	endpoint     *url.URL
	endpointOnce sync.Once
	endpointErr  error
}

func NewClient(apiKey, endpoint string, httpClient *http.Client, logger *slog.Logger, headers map[string]string) (*Client, error) {
	u, err := text.ValidateURL(endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{APIKey: apiKey, Endpoint: endpoint, Headers: headers, HTTP: httpClient, Logger: logger, endpoint: u}, nil
}

func (c *Client) resolveEndpoint() (*url.URL, error) {
	c.endpointOnce.Do(func() {
		if c.endpoint != nil {
			return
		}
		c.endpoint, c.endpointErr = text.ValidateURL(c.Endpoint)
	})
	return c.endpoint, c.endpointErr
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func (c *Client) prepareDo(req miniagent.Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	u, err := c.resolveEndpoint()
	if err != nil {
		return nil, nil, nil, err
	}
	req.Stream = false
	body, err := buildBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return c.client(), u, body, nil
}

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

func (c *Client) doOnce(ctx context.Context, client *http.Client, u *url.URL, body []byte) (miniagent.Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return miniagent.Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(httpReq)
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
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if readErr != nil {
		return miniagent.Response{}, true, -1, fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		retryable, retryAfter, err := httpError(resp, raw)
		return miniagent.Response{}, retryable, retryAfter, err
	}
	if len(raw) > maxResponseBodyBytes {
		return miniagent.Response{}, false, 0, fmt.Errorf("response exceeded %d bytes", maxResponseBodyBytes)
	}
	out, parseErr := parseResponse(raw)
	if parseErr != nil {
		return miniagent.Response{}, false, 0, parseErr
	}
	if c.Logger != nil {
		c.Logger.Info("llm call done", "duration_ms", callDur.Milliseconds(), "input_tokens", out.Usage.InputTokens, "output_tokens", out.Usage.OutputTokens, "tool_calls", len(out.ToolCalls), "finish_reason", out.FinishReason)
	}
	return out, false, 0, nil
}

func httpError(resp *http.Response, raw []byte) (bool, time.Duration, error) {
	if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
		return false, 0, fmt.Errorf("%w (status %d)", miniagent.ErrContextLength, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
		return false, 0, fmt.Errorf("%w (status %d)", miniagent.ErrThinkingUnsupported, resp.StatusCode)
	}
	if httpretry.ShouldRetryStatus(resp.StatusCode) {
		return true, httpretry.ParseRetryAfter(resp.Header), fmt.Errorf("llm returned %d", resp.StatusCode)
	}
	return false, 0, fmt.Errorf("llm returned %d", resp.StatusCode)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range c.Headers {
		if canonical := http.CanonicalHeaderKey(key); canonical == "Authorization" || canonical == "Content-Type" {
			continue
		}
		req.Header.Set(key, value)
	}
}
