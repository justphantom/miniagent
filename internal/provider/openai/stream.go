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

	"github.com/justphantom/miniagent/internal/miniagent"
)

// StreamClient 调 OpenAI 兼容 chat completions 端点（流式 SSE）。
// 流式的 *http.Client 不带总 Timeout（覆盖 body 读取会砍断长生成，P2-5），总时长交由 ctx 控制。
// 非流式走 ChatClient（P4 拆分）；SSE 解析见 stream_parse.go。
type StreamClient struct {
	APIKey            string
	ChatURL           string
	Headers           map[string]string // 自定义请求头，不覆盖 Authorization / Content-Type
	chatURL           *url.URL
	chatOnce          sync.Once
	chatErr           error
	HTTP              *http.Client
	Logger            *slog.Logger
	defaultClient     *http.Client // c.HTTP==nil 时缓存的流式 client（无 Timeout）
	defaultClientOnce sync.Once
}

// NewStreamClient 构造时 parse 并缓存 chatURL。headers 为 provider 自定义请求头，可为 nil。
func NewStreamClient(apiKey, chatURL string, httpClient *http.Client, logger *slog.Logger, headers map[string]string) (*StreamClient, error) {
	chat, err := miniagent.ValidateURL(chatURL)
	if err != nil {
		return nil, err
	}
	return &StreamClient{APIKey: apiKey, ChatURL: chatURL, chatURL: chat, HTTP: httpClient, Logger: logger, Headers: headers}, nil
}

// chatEndpoint 返回缓存的 chatURL（懒解析兜底直接构造，sync.Once 保证并发安全）。
func (c *StreamClient) chatEndpoint() (*url.URL, error) {
	c.chatOnce.Do(func() { cacheEndpoint(&c.chatURL, &c.chatErr, c.ChatURL) })
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	return c.chatURL, nil
}

// streamClient 返回流式 http.Client：注入的若有总 Timeout 会砍断 body（P2-5/P1-A），
// 改用其 Transport 另造无 Timeout client（保留代理，#2）；未注入则懒构造缓存。
func (c *StreamClient) streamClient() *http.Client {
	if c.HTTP != nil {
		if c.HTTP.Timeout > 0 {
			return &http.Client{Transport: c.HTTP.Transport}
		}
		return c.HTTP
	}
	c.defaultClientOnce.Do(func() { c.defaultClient = &http.Client{} })
	return c.defaultClient
}

// DoStream 流式调用 POST /v1/chat/completions（stream=true），onDelta 实时推增量，返回聚合 Response。
// onDelta 返回 error 时立即中止流并返回该 error。重试：pre-delta 阶段（client.Do 失败或非 200，尚未流出 delta）复用 shouldRetryStatus+退避+Retry-After；
// 进入 parseSSE（200，已流 delta）即不可撤回不重试（P2-4）。
func (c *StreamClient) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	if c.APIKey == "" {
		return miniagent.Response{}, errors.New("miniagent: api_key is empty")
	}
	u, err := c.chatEndpoint()
	if err != nil {
		return miniagent.Response{}, err
	}
	req.Stream = true
	body, err := buildChatBody(req)
	if err != nil {
		return miniagent.Response{}, fmt.Errorf("build request body: %w", err)
	}
	client := c.streamClient()
	if c.Logger != nil {
		c.Logger.Debug("llm stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	backoff := retryBaseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return miniagent.Response{}, err
		}
		// 每次重建 httpReq：body reader 上一轮已被消费，复用会发空 body。
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return miniagent.Response{}, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		// 注入 provider 自定义头；跳过 Authorization/Content-Type 防覆盖鉴权与内容类型
		//（与 client.go / models.go 同名循环对齐——此前本循环漏了跳过，与字段注释承诺相悖）。
		for k, v := range c.Headers {
			if ck := http.CanonicalHeaderKey(k); ck == "Authorization" || ck == "Content-Type" {
				continue
			}
			httpReq.Header.Set(k, v)
		}
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
			// 400/413 context 超限 / thinking：沿用非流式判定供 Run 降级，不重试；attempt>0 加 "after N retries" 前缀（P3 排错）。
			// §P1-C：状态门从仅 400 放宽到 400||413（与 client.go 对齐）。
			prefix := ""
			if attempt > 0 {
				prefix = fmt.Sprintf("after %d retries: ", attempt)
			}
			if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
				// raw 仅用于特征识别，不回显进 error——与非流式 client.go 对齐：恶意/调试代理可能在
				// 错误体回显 Authorization，error 经 emitRunError 进 NDJSON stdout 与 session jsonl 会泄漏 key。
				return miniagent.Response{}, fmt.Errorf("%s%w（状态 %d）", prefix, miniagent.ErrContextLength, resp.StatusCode)
			}
			if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
				return miniagent.Response{}, fmt.Errorf("%s%w（状态 %d）", prefix, miniagent.ErrThinkingUnsupported, resp.StatusCode)
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
		return func() (miniagent.Response, error) { // HTTP 200：parseSSE 流 delta。IIFE 内 defer Close：onDelta panic（被 callLLMOnce recover）时仍关 body；函数级 defer 跨重试堆积故每次循环内联。
			defer func() { _ = resp.Body.Close() }()
			return parseSSE(resp.Body, onDelta)
		}()
	}
	return miniagent.Response{}, errors.New("llm stream retry loop exited unexpectedly")
}
