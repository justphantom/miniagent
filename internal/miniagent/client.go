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

const (
	maxChatBodyBytes = 4 << 20 // 4 MiB；恰好达到不截断，多 1 字节即报错
	// 重试：仅对瞬时故障（429/5xx + 网络错误）生效，最多 maxRetries 次。
	// 取值依据：LLM 端点 429/503 抖动通常在数秒内自愈，2 次足以覆盖典型尖刺，
	// 同时避免在真故障下放大下游压力（雪崩）。
	maxRetries     = 2
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second // 单次退火上限，含 Retry-After 解析值
)

// HTTPClient calls an OpenAI-compatible chat completions endpoint via net/http.
type HTTPClient struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
	Logger  *slog.Logger
}

// Do 调用 POST {BaseURL}/v1/chat/completions（非流式），解析 choices[0] /
// usage / finish_reason。响应 body 上限 maxChatBodyBytes，越界报错。
//
// 重试策略：429/500/502/503/504 与网络错误自动重试 maxRetries 次，退避按
// retryBaseDelay * 2^attempt；若响应带 Retry-After（秒数或 HTTP-date），
// 用它取代退避（仍受 retryMaxDelay 封顶）。其他 4xx / 解析错误 / 超大
// body 立即返回。重试可被 ctx 取消。
func (c *HTTPClient) Do(ctx context.Context, req Request) (Response, error) {
	client, u, body, err := c.prepareDo(req)
	if err != nil {
		return Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("llm request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	backoff := retryBaseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		resp, retryable, retryAfter, err := c.doOnce(ctx, client, u, body)
		if err == nil {
			return resp, nil
		}
		if !retryable || attempt == maxRetries {
			// 用尽重试或不可重试错误：补"已重试 N 次"上下文便于排错。
			if attempt > 0 {
				return Response{}, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return Response{}, err
		}
		delay := backoff
		if retryAfter > 0 {
			delay = retryAfter
		}
		if delay > retryMaxDelay {
			delay = retryMaxDelay
		}
		if c.Logger != nil {
			c.Logger.Warn("llm call failed, retrying", "failed_attempt", attempt+1, "delay_ms", delay.Milliseconds(), "error", err)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
		backoff *= 2
	}
	// 循环正常退出（不会到达，所有路径已在循环内 return）；保留兜底。
	return Response{}, errors.New("llm retry loop exited unexpectedly")
}

// doOnce 执行单次 HTTP 调用并解析。retryable 表示错误是否值得重试；
// retryAfter 是 Retry-After 头解析出的等待时长（0 表示未提供）。
func (c *HTTPClient) doOnce(ctx context.Context, client *http.Client, u *url.URL, body []byte) (Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	// 重定向安全：依赖标准库默认 CheckRedirect——跨域重定向前自动剥离
	// Authorization 等敏感头。若未来自定义 CheckRedirect，必须保留该语义。
	httpReq.Header.Set("Content-Type", "application/json")

	callStart := time.Now()
	resp, err := client.Do(httpReq)
	callDur := time.Since(callStart)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm request failed", "error", err, "duration_ms", callDur.Milliseconds())
		}
		// 网络层错误（连接拒绝/DNS/超时）：值得重试，无 Retry-After。
		return Response{}, true, 0, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
	if rerr != nil {
		return Response{}, true, 0, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxChatBodyBytes {
		return Response{}, false, 0, fmt.Errorf("response exceeded %d bytes", maxChatBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("llm returned %d: %s", resp.StatusCode, truncate(string(raw), 500, "…"))
		if shouldRetryStatus(resp.StatusCode) {
			return Response{}, true, parseRetryAfter(resp.Header), errors.New(msg)
		}
		return Response{}, false, 0, errors.New(msg)
	}
	out, perr := parseChatResponse(raw)
	if perr != nil {
		return Response{}, false, 0, perr
	}
	if c.Logger != nil {
		c.Logger.Info("llm call done", "duration_ms", callDur.Milliseconds(), "input_tokens", out.Usage.InputTokens, "output_tokens", out.Usage.OutputTokens, "tool_calls", len(out.ToolCalls), "finish_reason", out.FinishReason)
	}
	return out, false, 0, nil
}

func shouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// parseRetryAfter 解析 Retry-After 头：秒数（RFC 7231 §7.1.3）或 HTTP-date。
// 解析失败或未提供返回 0。返回值不做上限封顶（封顶在调用处）。
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// endpoint 解析 BaseURL 并拼接 path；c.HTTP 为 nil 时用 defaultTimeout 的默认 client。
func (c *HTTPClient) endpoint(path string, defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	trimmed := strings.TrimRight(c.BaseURL, "/")
	base, err := url.Parse(trimmed)
	if err != nil {
		return nil, nil, fmt.Errorf("miniagent: base_url %q 解析失败：%w（需 http(s)://host[:port]）", c.BaseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, nil, fmt.Errorf("miniagent: base_url %q 缺少 scheme 或 host（应为 http(s)://host[:port]）", c.BaseURL)
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

// chatCompletionResponse 只摘出循环需要的字段：首条 choice 的 message
// （content + tool_calls）、finish_reason、usage。
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func parseChatResponse(raw []byte) (Response, error) {
	var v chatCompletionResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		return Response{}, fmt.Errorf("parse response: %w", err)
	}
	out := Response{}
	if len(v.Choices) == 0 {
		// 空 choices 是端点异常（内容过滤/代理故障），静默零值会让上层把
		// 它当作"成功的空回答"（退出码 0、text 为空），必须报错。
		return Response{}, errors.New("llm response has no choices")
	}
	ch := v.Choices[0]
	out.Text = ch.Message.Content
	out.FinishReason = ch.FinishReason
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	if v.Usage != nil {
		out.Usage = Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}
