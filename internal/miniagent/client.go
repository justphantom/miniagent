package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"log/slog"
)

// HTTPClient calls an OpenAI-compatible chat completions endpoint via net/http.
type HTTPClient struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
	Logger  *slog.Logger
}

var retryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Do posts req to {BaseURL}/v1/chat/completions and parses the response.
func (c *HTTPClient) Do(ctx context.Context, req Request) (Response, error) {
	if c.APIKey == "" {
		return Response{}, fmt.Errorf("miniagent: api_key is empty")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	body, err := buildChatBody(req)
	if err != nil {
		return Response{}, fmt.Errorf("build request body: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/v1/chat/completions"
	if c.Logger != nil {
		c.Logger.Debug("http request", "url", url, "model", req.Model, "messages", len(req.Messages))
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, status, err := c.doOnce(ctx, client, url, body)
		if err == nil && status == http.StatusOK {
			if c.Logger != nil {
				c.Logger.Debug("http response", "status", status, "bytes", len(raw), "attempt", attempt)
			}
			return parseChatResponse(raw)
		}
		if err != nil {
			lastErr = fmt.Errorf("llm request: %w", err)
		} else {
			lastErr = fmt.Errorf("llm returned %d: %s", status, truncate(string(raw), 500, "…"))
		}
		if err != nil || !retryableStatus(status) || attempt >= len(retryDelays) {
			return Response{}, lastErr
		}
		delay := retryDelays[attempt]
		if ra := parseRetryAfter(raw); ra > delay {
			delay = ra
		}
		const maxRetryDelay = 60 * time.Second
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
}

func (c *HTTPClient) doOnce(ctx context.Context, client *http.Client, url string, body []byte) (raw []byte, status int, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return raw, resp.StatusCode, fmt.Errorf("read response: %w", rerr)
	}
	return raw, resp.StatusCode, nil
}

// ListModels calls GET {BaseURL}/v1/models and returns the model ids.
func (c *HTTPClient) ListModels(ctx context.Context) ([]string, error) {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: %d", resp.StatusCode)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

func parseRetryAfter(body []byte) time.Duration {
	var v struct {
		Error struct {
			RetryAfter float64 `json:"retry_after"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil && v.Error.RetryAfter > 0 {
		return time.Duration(v.Error.RetryAfter * float64(time.Second))
	}
	return 0
}
