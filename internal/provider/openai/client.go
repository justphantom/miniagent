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

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

const maxChatBodyBytes = 4 << 20 // 4 MiB；恰好达到不截断，多 1 字节即报错

// ChatClient 调 OpenAI 兼容 chat completions 端点（非流式）+ models 列表。
// 懒解析（直接 struct 构造的测试路径）用 sync.Once 保护，确保并发 Do 无竞争（修复 R4）。
// 流式走 StreamClient（P4 拆分）；本 client 的 *http.Client 带总 Timeout 兜底防单次调用挂死。
type ChatClient struct {
	APIKey            string
	ChatURL           string
	ModelsURL         string
	Headers           map[string]string // 自定义请求头，不覆盖 Authorization / Content-Type
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
}

// NewChatClient 构造时 parse 并缓存 chatURL/modelsURL（审查 v3 #10）。modelsURL 可空
// （ListAllModels 静态回落时不 GET）。headers 为 provider 自定义请求头，可为 nil。
func NewChatClient(apiKey, chatURL, modelsURL string, httpClient *http.Client, logger *slog.Logger, headers map[string]string) (*ChatClient, error) {
	chat, err := miniagent.ValidateURL(chatURL)
	if err != nil {
		return nil, err
	}
	c := &ChatClient{APIKey: apiKey, ChatURL: chatURL, ModelsURL: modelsURL, chatURL: chat, HTTP: httpClient, Logger: logger, Headers: headers}
	if modelsURL != "" {
		m, err := miniagent.ValidateURL(modelsURL)
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
	u, err := miniagent.ValidateURL(raw)
	if err != nil {
		*errp = err
		return
	}
	*dst = u
}

// chatEndpoint 返回缓存的 chatURL（懒解析兜底直接构造，sync.Once 保证并发安全）。
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

// client 返回非流式 http.Client。c.HTTP!=nil 沿用注入；否则懒构造并缓存带 defaultTimeout 的 client（P3-5）。
func (c *ChatClient) client(defaultTimeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.defaultClientOnce.Do(func() { c.defaultClient = &http.Client{Timeout: defaultTimeout} })
	return c.defaultClient
}

// Do 调用 POST ChatURL（非流式），解析 choices[0] / usage / finish_reason。响应 body
// 上限 maxChatBodyBytes，越界报错。
//
// 重试策略：429/500/502/503/504 与网络错误自动重试 maxRetries 次，退避按
// retryBaseDelay * 2^attempt；若响应带 Retry-After（秒数或 HTTP-date），用它取代退避
// （仍受 retryMaxDelay 封顶）。其他 4xx / 解析错误 / 超大 body 立即返回。重试可被 ctx 取消。
func (c *ChatClient) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	client, u, body, err := c.prepareDo(req)
	if err != nil {
		return miniagent.Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("llm request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	backoff := retryBaseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return miniagent.Response{}, err
		}
		resp, retryable, retryAfter, err := c.doOnce(ctx, client, u, body)
		if err == nil {
			return resp, nil
		}
		if !retryable || attempt == maxRetries {
			// 用尽重试或不可重试错误：补"已重试 N 次"上下文便于排错。
			if attempt > 0 {
				return miniagent.Response{}, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return miniagent.Response{}, err
		}
		delay := capRetryDelay(backoff, retryAfter)
		if c.Logger != nil {
			c.Logger.Warn("llm call failed, retrying", "failed_attempt", attempt+1, "delay_ms", delay.Milliseconds(), "error", err)
		}
		if waitErr := sleepCtx(ctx, delay); waitErr != nil {
			return miniagent.Response{}, waitErr
		}
		backoff *= 2
	}
	// 循环正常退出（不会到达，所有路径已在循环内 return）；保留兜底。
	return miniagent.Response{}, errors.New("llm retry loop exited unexpectedly")
}

// doOnce 执行单次 HTTP 调用并解析。retryable 表示错误是否值得重试；
// retryAfter 是 Retry-After 头解析出的等待时长（-1 表示未提供）。
func (c *ChatClient) doOnce(ctx context.Context, client *http.Client, u *url.URL, body []byte) (miniagent.Response, bool, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return miniagent.Response{}, false, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	// 重定向安全：依赖标准库默认 CheckRedirect——跨域重定向前自动剥离 Authorization
	// 等敏感头。若未来自定义 CheckRedirect，必须保留该语义。
	httpReq.Header.Set("Content-Type", "application/json")
	// 注入 provider 自定义头；Authorization / Content-Type 不在此覆盖。
	for k, v := range c.Headers {
		httpReq.Header.Set(k, v)
	}

	callStart := time.Now()
	resp, err := client.Do(httpReq)
	callDur := time.Since(callStart)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm request failed", "error", err, "duration_ms", callDur.Milliseconds())
		}
		// 网络层错误（连接拒绝/DNS/超时）：值得重试，无 Retry-After。
		return miniagent.Response{}, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
	if rerr != nil {
		return miniagent.Response{}, true, -1, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxChatBodyBytes {
		return miniagent.Response{}, false, 0, fmt.Errorf("response exceeded %d bytes", maxChatBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// context 超限（400/413 + 特征词）单列：上层 Run 据此做一次历史收紧重试。
		// §P1-C：状态门从仅 400 放宽到 400||413（Anthropic request_too_large 走 413）。
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge) && miniagent.IsContextLengthError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w: %s", miniagent.ErrContextLength, text.Truncate(string(raw), 500, "…"))
		}
		// thinking 参数不被支持（400 + 特征词）：callLLMWithDowngrade 据此去字段重试一次（审查 v2 #7）。
		if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
			return miniagent.Response{}, false, 0, fmt.Errorf("%w: %s", miniagent.ErrThinkingUnsupported, text.Truncate(string(raw), 500, "…"))
		}
		msg := fmt.Sprintf("llm returned %d: %s", resp.StatusCode, text.Truncate(string(raw), 500, "…"))
		if shouldRetryStatus(resp.StatusCode) {
			return miniagent.Response{}, true, parseRetryAfter(resp.Header), errors.New(msg)
		}
		return miniagent.Response{}, false, 0, errors.New(msg)
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
	req.Stream = false // Do 必非流式，防御调用方误设
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
}

// Provider 是 OpenAI 兼容 LLM 的默认实现：把 ChatClient（Do，非流式）与 StreamClient
// （DoStream，流式）组合，满足 miniagent.LLM 接口。cmd 装配它喂给 Run；自定义 provider
// 实现 miniagent.LLM 即可替换，核心 Run 零改动（这是「provider 作为外挂」的默认实现）。
// Stream 仅在 cfg.Stream=true 时被调，非流式场景可为 nil。
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
