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
	"strings"
	"sync"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/provider/httpretry"
	"github.com/justphantom/miniagent/internal/text"
)

type StreamClient struct {
	APIKey                  string
	Endpoint                string
	Headers                 map[string]string
	StreamAllowUnterminated bool
	HTTP                    *http.Client
	Logger                  *slog.Logger
	endpoint                *url.URL
	endpointOnce            sync.Once
	endpointErr             error
	noTimeoutClient         *http.Client
	noTimeoutOnce           sync.Once
}

func NewStreamClient(apiKey, endpoint string, httpClient *http.Client, logger *slog.Logger, headers map[string]string) (*StreamClient, error) {
	u, err := text.ValidateURL(endpoint)
	if err != nil {
		return nil, err
	}
	return &StreamClient{APIKey: apiKey, Endpoint: endpoint, Headers: headers, HTTP: httpClient, Logger: logger, endpoint: u}, nil
}

func (c *StreamClient) resolveEndpoint() (*url.URL, error) {
	c.endpointOnce.Do(func() {
		if c.endpoint != nil {
			return
		}
		c.endpoint, c.endpointErr = text.ValidateURL(c.Endpoint)
	})
	return c.endpoint, c.endpointErr
}

func (c *StreamClient) streamClient() *http.Client {
	if c.HTTP != nil {
		if c.HTTP.Timeout > 0 {
			c.noTimeoutOnce.Do(func() {
				c.noTimeoutClient = &http.Client{Transport: c.HTTP.Transport}
			})
			return c.noTimeoutClient
		}
		return c.HTTP
	}
	return &http.Client{}
}

func (c *StreamClient) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	if c.APIKey == "" {
		return miniagent.Response{}, errors.New("miniagent: api_key is empty")
	}
	u, err := c.resolveEndpoint()
	if err != nil {
		return miniagent.Response{}, err
	}
	req.Stream = true
	body, err := buildBody(req)
	if err != nil {
		return miniagent.Response{}, fmt.Errorf("build request body: %w", err)
	}
	if c.Logger != nil {
		c.Logger.Debug("llm stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	client := c.streamClient()
	deltaSent := 0
	wrappedOnDelta := func(delta miniagent.Delta) error {
		deltaSent++
		if onDelta == nil {
			return nil
		}
		return onDelta(delta)
	}
	backoff := httpretry.RetryBaseDelay
	for attempt := 0; attempt <= httpretry.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return miniagent.Response{}, err
		}
		deltaSent = 0
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return miniagent.Response{}, fmt.Errorf("build request: %w", err)
		}
		c.setHeaders(httpReq)
		resp, err := client.Do(httpReq)
		if err != nil {
			if attempt == httpretry.MaxRetries {
				return miniagent.Response{}, fmt.Errorf("after %d retries: llm request: %w", attempt, err)
			}
			if waitErr := httpretry.SleepCtx(ctx, httpretry.CapRetryDelay(backoff, -1)); waitErr != nil {
				return miniagent.Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
			_ = resp.Body.Close()
			retryable, retryAfter, statusErr := httpError(resp, raw)
			if !retryable || attempt == httpretry.MaxRetries {
				if attempt > 0 {
					return miniagent.Response{}, fmt.Errorf("after %d retries: %w", attempt, statusErr)
				}
				return miniagent.Response{}, statusErr
			}
			if waitErr := httpretry.SleepCtx(ctx, httpretry.CapRetryDelay(backoff, retryAfter)); waitErr != nil {
				return miniagent.Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		result, parseErr := func() (miniagent.Response, error) {
			defer func() { _ = resp.Body.Close() }()
			return parseSSE(resp.Body, wrappedOnDelta)
		}()
		if parseErr == nil {
			return result, nil
		}
		if c.StreamAllowUnterminated && errors.Is(parseErr, errStreamUnterminated) {
			if c.Logger != nil {
				c.Logger.Warn("stream ended without terminal event; accepted partial response")
			}
			return result, nil
		}
		if deltaSent == 0 && isTransientStreamError(parseErr) && attempt < httpretry.MaxRetries {
			if waitErr := httpretry.SleepCtx(ctx, httpretry.CapRetryDelay(backoff, -1)); waitErr != nil {
				return miniagent.Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		return miniagent.Response{}, parseErr
	}
	return miniagent.Response{}, errors.New("llm stream retry loop exited unexpectedly")
}

func (c *StreamClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range c.Headers {
		if canonical := http.CanonicalHeaderKey(key); canonical == "Authorization" || canonical == "Content-Type" {
			continue
		}
		req.Header.Set(key, value)
	}
}

func isTransientStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "read sse:") || strings.Contains(msg, "stream ended without terminal response event")
}
