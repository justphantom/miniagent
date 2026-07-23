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

// ListModels calls GET {BaseURL}/v1/models and returns the model ids.
// 与 chat 共用重试策略与 body 上限，避免异常端点返回超大 body 拖垮内存。
func (c *HTTPClient) ListModels(ctx context.Context) ([]string, error) {
	client, u, err := c.prepareListModels()
	if err != nil {
		return nil, err
	}
	return c.executeListModels(ctx, client, u)
}

// doRaw 执行单次请求并读尽 body（上限 limit，多读 1 字节判定超限：
// 恰好达到上限不应误报截断）。body 为 nil 时不带 Content-Type（GET）。
func (c *HTTPClient) doRaw(ctx context.Context, client *http.Client, method, url string, body []byte, limit int64) (raw []byte, status int, retryAfter time.Duration, err error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if rerr != nil {
		return raw, resp.StatusCode, 0, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > limit {
		return raw[:limit], resp.StatusCode, 0, fmt.Errorf("response exceeded %d bytes and was truncated", limit)
	}
	return raw, resp.StatusCode, parseRetryAfterHeader(resp.Header.Get("Retry-After")), nil
}

// endpoint 解析 BaseURL 并拼接 path；c.HTTP 为 nil 时用 defaultTimeout 的默认 client。
func (c *HTTPClient) endpoint(path string, defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, nil, fmt.Errorf("miniagent: base_url %q is invalid", c.BaseURL)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return client, base.JoinPath(path), nil
}

func (c *HTTPClient) prepareDo(req Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	client, u, err := c.endpoint("/v1/chat/completions", 120*time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
}

func (c *HTTPClient) prepareListModels() (*http.Client, string, error) {
	client, u, err := c.endpoint("/v1/models", 30*time.Second)
	if err != nil {
		return nil, "", err
	}
	return client, u.String(), nil
}

func (c *HTTPClient) executeListModels(ctx context.Context, client *http.Client, url string) ([]string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, status, _, err := c.doRaw(ctx, client, http.MethodGet, url, nil, maxModelsBodyBytes)
		if err == nil && status == http.StatusOK {
			return parseModels(raw)
		}

		lastErr = formatListModelsErr(status, err)
		if err != nil {
			return nil, lastErr
		}
		ok, rerr := retryIfPossible(ctx, attempt, status, 0, raw)
		if rerr != nil {
			return nil, rerr
		}
		if !ok {
			return nil, lastErr
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

// retryIfPossible 在状态码可重试且重试次数未用尽时退避等待，返回是否应继续。
func retryIfPossible(ctx context.Context, attempt, status int, raHeader time.Duration, raw []byte) (bool, error) {
	if !retryableStatus(status) || attempt >= len(retryDelays) {
		return false, nil
	}
	if err := sleepRetry(ctx, attempt, raHeader, raw); err != nil {
		return false, err
	}
	return true, nil
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
