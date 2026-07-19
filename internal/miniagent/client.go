package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	client, u, body, err := c.prepareDo(req)
	if err != nil {
		return Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("http request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	return c.executeChat(ctx, client, u.String(), body)
}

// ListModels calls GET {BaseURL}/v1/models and returns the model ids.
// 与 Do 共用重试策略与 body 上限，避免异常端点返回超大 body 拖垮内存。
func (c *HTTPClient) ListModels(ctx context.Context) ([]string, error) {
	client, u, err := c.prepareListModels()
	if err != nil {
		return nil, err
	}
	return c.executeListModels(ctx, client, u)
}

func (c *HTTPClient) doOnce(ctx context.Context, client *http.Client, url string, body []byte) (raw []byte, status int, retryAfter time.Duration, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 多读 1 字节判定是否超限：恰好 1MiB 不应误报截断。
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if rerr != nil {
		return raw, resp.StatusCode, 0, fmt.Errorf("read response: %w", rerr)
	}
	if len(raw) > 1<<20 {
		return raw[:1<<20], resp.StatusCode, 0, errors.New("response exceeded 1 MiB limit and was truncated")
	}
	return raw, resp.StatusCode, parseRetryAfterHeader(resp.Header.Get("Retry-After")), nil
}

// doGetOnce 是 ListModels 的单次 GET，返回原始 body 用于复用 parseRetryAfter。
func (c *HTTPClient) doGetOnce(ctx context.Context, client *http.Client, url string) (raw []byte, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes+1))
	if rerr != nil {
		return raw, resp.StatusCode, fmt.Errorf("read response: %w", rerr)
	}
	if len(raw) > maxModelsBodyBytes {
		return raw[:maxModelsBodyBytes], resp.StatusCode, fmt.Errorf("models response exceeded %d bytes", maxModelsBodyBytes)
	}
	return raw, resp.StatusCode, nil
}

func (c *HTTPClient) prepareDo(req Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, nil, nil, fmt.Errorf("miniagent: base_url %q is invalid", c.BaseURL)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, base.JoinPath("/v1/chat/completions"), body, nil
}

func (c *HTTPClient) executeChat(ctx context.Context, client *http.Client, url string, body []byte) (Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, status, raHeader, err := c.doOnce(ctx, client, url, body)
		if err == nil && status == http.StatusOK {
			if c.Logger != nil {
				c.Logger.Debug("http response", "status", status, "bytes", len(raw), "attempt", attempt)
			}
			return parseChatResponse(raw)
		}

		lastErr = formatDoErr(raw, status, err)
		if err != nil || !retryableStatus(status) || attempt >= len(retryDelays) {
			return Response{}, lastErr
		}

		if err := sleepRetry(ctx, attempt, raHeader, raw); err != nil {
			return Response{}, err
		}
	}
}

func (c *HTTPClient) prepareListModels() (*http.Client, string, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, "", fmt.Errorf("miniagent: base_url %q is invalid", c.BaseURL)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return client, base.JoinPath("/v1/models").String(), nil
}

func (c *HTTPClient) executeListModels(ctx context.Context, client *http.Client, url string) ([]string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, status, err := c.doGetOnce(ctx, client, url)
		if err == nil && status == http.StatusOK {
			return parseModels(raw)
		}

		lastErr = formatListModelsErr(status, err)
		if err != nil || !retryableStatus(status) || attempt >= len(retryDelays) {
			return nil, lastErr
		}

		if err := sleepRetry(ctx, attempt, 0, raw); err != nil {
			return nil, err
		}
	}
}

const maxModelsBodyBytes = 4 << 20 // 4 MiB

func parseModels(raw []byte) ([]string, error) {
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
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

func formatDoErr(raw []byte, status int, err error) error {
	if err != nil {
		return fmt.Errorf("llm request: %w", err)
	}
	return fmt.Errorf("llm returned %d: %s", status, truncate(string(raw), 500, "…"))
}

func formatListModelsErr(status int, err error) error {
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}
	return fmt.Errorf("list models: %d", status)
}

func sleepRetry(ctx context.Context, attempt int, raHeader time.Duration, raw []byte) error {
	delay := retryDelays[attempt]
	// Retry-After 优先：HTTP 头是标准做法，body 里的 retry_after 是部分厂商扩展。
	// 取 header 与 body 的较大值，保守退避。
	delay = max(delay, raHeader, parseRetryAfter(raw))
	const maxRetryDelay = 60 * time.Second
	delay = min(delay, maxRetryDelay)
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// parseRetryAfterHeader 解析标准 HTTP Retry-After 头：可能是秒数或 HTTP-date。
// 参考 RFC 7231 §7.1.3。
func parseRetryAfterHeader(val string) time.Duration {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}
	// 纯数字 = 秒。
	if n, err := strconv.Atoi(val); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	// HTTP-date 格式。
	if t, err := http.ParseTime(val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
