package miniagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"
)

const (
	maxChatBodyBytes = 4 << 20 // 4 MiB；恰好达到不截断，多 1 字节即报错
	// 重试：仅对瞬时故障（429/5xx + 网络错误）生效，maxRetries 次。端点 429/503 抖动
	// 数秒内自愈，2 次覆盖典型尖刺；避免真故障下放大下游压力（雪崩）。
	maxRetries     = 2
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second // 单次退火上限，含 Retry-After 解析值
)

// HTTPClient calls an OpenAI-compatible chat completions endpoint via net/http.
// 懒解析（直接 struct 构造的测试路径）用 sync.Once 保护，确保并发 Do/DoStream 无竞争（修复 R4）。
type HTTPClient struct {
	APIKey            string
	ChatURL           string
	ModelsURL         string
	chatURL           *url.URL
	chatOnce          sync.Once
	chatErr           error
	modelsURL         *url.URL
	modelsOnce        sync.Once
	modelsErr         error
	HTTP              *http.Client
	Logger            *slog.Logger
	defaultClient     *http.Client // c.HTTP==nil 时缓存的非流式 client（P3-5）
	defaultClientOnce sync.Once
	streamClient      *http.Client // 流式专用 client（无总 Timeout，P2-5）
	streamClientOnce  sync.Once
}

// validateURL 解析并校验 raw 为合法 http(s) URL（含 scheme+host）。
func validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url %q 解析失败：%w（需 http(s)://host[:port][/path]）", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("url %q 缺少 scheme 或 host（应为 http(s)://host[:port]）", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url %q 的 scheme %q 不支持（仅 http/https）", raw, u.Scheme)
	}
	return u, nil
}

// NewHTTPClient 构造时 parse 并缓存 chatURL/modelsURL（审查 v3 #10）。modelsURL 可空
// （ListAvailableModels 静态回落时不 GET）。
func NewHTTPClient(apiKey, chatURL, modelsURL string, httpClient *http.Client, logger *slog.Logger) (*HTTPClient, error) {
	chat, err := validateURL(chatURL)
	if err != nil {
		return nil, err
	}
	c := &HTTPClient{APIKey: apiKey, ChatURL: chatURL, ModelsURL: modelsURL, chatURL: chat, HTTP: httpClient, Logger: logger}
	if modelsURL != "" {
		m, err := validateURL(modelsURL)
		if err != nil {
			return nil, err
		}
		c.modelsURL = m
	}
	return c, nil
}

// cacheEndpoint 解析 raw 缓存进 dst（已设则不动），失败缓存进 errp。调用方在
// sync.Once.Do 内调用以并发安全（chat/models 复用，修复 R4）。
func cacheEndpoint(dst **url.URL, errp *error, raw string) {
	if *dst != nil {
		return
	}
	u, err := validateURL(raw)
	if err != nil {
		*errp = err
		return
	}
	*dst = u
}

// chatEndpoint 返回缓存的 chatURL（懒解析兜底直接构造，sync.Once 保证并发安全）。
func (c *HTTPClient) chatEndpoint(defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	c.chatOnce.Do(func() { cacheEndpoint(&c.chatURL, &c.chatErr, c.ChatURL) })
	if c.chatErr != nil {
		return nil, nil, c.chatErr
	}
	return c.client(defaultTimeout), c.chatURL, nil
}

func (c *HTTPClient) modelsEndpoint(defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	c.modelsOnce.Do(func() {
		if c.modelsURL == nil && c.ModelsURL == "" {
			c.modelsErr = errors.New("miniagent: models_url 未配置")
			return
		}
		cacheEndpoint(&c.modelsURL, &c.modelsErr, c.ModelsURL)
	})
	if c.modelsErr != nil {
		return nil, nil, c.modelsErr
	}
	return c.client(defaultTimeout), c.modelsURL, nil
}

// Do 调用 POST ChatURL（非流式），解析 choices[0] / usage / finish_reason。响应 body
// 上限 maxChatBodyBytes，越界报错。
//
// 重试策略：429/500/502/503/504 与网络错误自动重试 maxRetries 次，退避按
// retryBaseDelay * 2^attempt；若响应带 Retry-After（秒数或 HTTP-date），用它取代退避
// （仍受 retryMaxDelay 封顶）。其他 4xx / 解析错误 / 超大 body 立即返回。重试可被 ctx 取消。
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
		delay := capRetryDelay(backoff, retryAfter)
		if c.Logger != nil {
			c.Logger.Warn("llm call failed, retrying", "failed_attempt", attempt+1, "delay_ms", delay.Milliseconds(), "error", err)
		}
		if waitErr := sleepCtx(ctx, delay); waitErr != nil {
			return Response{}, waitErr
		}
		backoff *= 2
	}
	// 循环正常退出（不会到达，所有路径已在循环内 return）；保留兜底。
	return Response{}, errors.New("llm retry loop exited unexpectedly")
}

// doOnce 执行单次 HTTP 调用并解析。retryable 表示错误是否值得重试；
// retryAfter 是 Retry-After 头解析出的等待时长（-1 表示未提供）。
func (c *HTTPClient) doOnce(ctx context.Context, client *http.Client, u *url.URL, body []byte) (Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	// 重定向安全：依赖标准库默认 CheckRedirect——跨域重定向前自动剥离 Authorization
	// 等敏感头。若未来自定义 CheckRedirect，必须保留该语义。
	httpReq.Header.Set("Content-Type", "application/json")

	callStart := time.Now()
	resp, err := client.Do(httpReq)
	callDur := time.Since(callStart)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm request failed", "error", err, "duration_ms", callDur.Milliseconds())
		}
		// 网络层错误（连接拒绝/DNS/超时）：值得重试，无 Retry-After。
		return Response{}, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
	if rerr != nil {
		return Response{}, true, -1, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxChatBodyBytes {
		return Response{}, false, 0, fmt.Errorf("response exceeded %d bytes", maxChatBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// context 超限（400 + 特征词）单列：上层 Run 据此做一次历史收紧重试。
		if resp.StatusCode == http.StatusBadRequest && isContextLengthError(raw) {
			return Response{}, false, 0, fmt.Errorf("%w: %s", ErrContextLength, truncate(string(raw), 500, "…"))
		}
		// thinking 参数不被支持（400 + 特征词）：callLLM 据此去字段重试一次（审查 v2 #7）。
		if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
			return Response{}, false, 0, fmt.Errorf("%w: %s", ErrThinkingUnsupported, truncate(string(raw), 500, "…"))
		}
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

// isContextLengthError 启发式识别 context 超限的 400 响应：不同厂商措辞不一
// （OpenAI: "maximum context length" / "context_length_exceeded"；其他: "context window"）。
// 小写匹配命中任一即认定。误判的最坏后果是触发一次无谓的历史收紧重试，可接受。
func isContextLengthError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"context_length", "context length", "maximum context", "context window"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isThinkingError 启发式识别 thinking 参数（reasoning_effort 等）不被支持的 400：跨供应商
// 措辞不一（"reasoning_effort"/"unknown parameter"/"unrecognized"）。宽松识别——误判只会触发
// 一次无 thinking 重试，无害（审查 v2 #7）。
func isThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"reasoning_effort", "reasoning_effort_level", "unknown parameter", "unrecognized", "unexpected argument"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseRetryAfter 解析 Retry-After 头：秒数（RFC 7231 §7.1.3）或 HTTP-date。
// 未提供或解析失败返回 -1（哨兵），以区分显式 "Retry-After: 0"——后者语义为立即重试。
// 返回值不做上限封顶（封顶在调用处）。
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return -1
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		// HTTP-date 已成过去：语义等同"立即可重试"，返回 0（区别于 -1 走 backoff）。P3-3。
		return 0
	}
	return -1
}

func (c *HTTPClient) prepareDo(req Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	client, u, err := c.chatEndpoint(120 * time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Stream = false // Do 必非流式，防御调用方误设
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
}
