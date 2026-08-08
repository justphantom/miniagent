package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent/config"
)

// ListModels 调 GET ModelsURL，返回 id 列表。复用 ChatClient.modelsEndpoint/鉴权。
// 对 429/5xx 与网络错误自动重试 maxRetries 次，退避策略与 Do 一致。
const maxModelsBodyBytes = 1 << 20 // 1 MiB；models 列表远小于 chat body，封顶防 OOM

// ListModels 调 GET ModelsURL，返回 id 列表。复用 ChatClient.modelsEndpoint/鉴权。
// 对 429/5xx 与网络错误自动重试 maxRetries 次，退避策略与 Do 一致。
func (c *ChatClient) ListModels(ctx context.Context) ([]string, error) {
	client, u, err := c.modelsEndpoint(30 * time.Second)
	if err != nil {
		return nil, err
	}
	backoff := retryBaseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids, retryable, retryAfter, err := c.listModelsOnce(ctx, client, u)
		if err == nil {
			return ids, nil
		}
		if !retryable || attempt == maxRetries {
			if attempt > 0 {
				return nil, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return nil, err
		}
		delay := capRetryDelay(backoff, retryAfter)
		if c.Logger != nil {
			c.Logger.Warn("list models failed, retrying", "failed_attempt", attempt+1, "delay_ms", delay.Milliseconds(), "error", err)
		}
		if waitErr := sleepCtx(ctx, delay); waitErr != nil {
			return nil, waitErr
		}
		backoff *= 2
	}
	return nil, errors.New("list models retry loop exited unexpectedly")
}

// listModelsOnce 单次 GET /models，返回 (ids, retryable, retryAfter, error)。retryAfter 解析自
// Retry-After 头（-1=未提供），供 ListModels 重试退避与 ChatClient.Do 对齐（此前恒 -1 致忽略上游 backpressure）。
func (c *ChatClient) listModelsOnce(ctx context.Context, client *http.Client, u *url.URL) ([]string, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	// 注入 provider 自定义头；跳过 Authorization/Content-Type 防覆盖鉴权（与 ChatClient.doOnce 一致）。
	for k, v := range c.Headers {
		if ck := http.CanonicalHeaderKey(k); ck == "Authorization" || ck == "Content-Type" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 排空 body 供连接 keepalive 复用（重试可能复用同一连接）；封顶 maxModelsBodyBytes 防
		// 恶意/异常端点慢 trickle 把每次重试拖到 client timeout（与 200 路径的 LimitReader 对齐）。
		// 不回显响应体——恶意/调试代理可能在错误体回显 URL/Authorization，error 经 -list-models stdout 泄漏 key
		//（与 doOnce 一致，仅回显状态码）。
		retryAfter := parseRetryAfter(resp.Header)
		if _, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxModelsBodyBytes+1)); readErr != nil {
			return nil, shouldRetryStatus(resp.StatusCode), retryAfter, fmt.Errorf("llm returned %d: read body: %w", resp.StatusCode, readErr)
		}
		return nil, shouldRetryStatus(resp.StatusCode), retryAfter, fmt.Errorf("llm returned %d", resp.StatusCode)
	}
	// 200 路径封顶防 OOM（与 doOnce 的 LimitReader(maxChatBodyBytes+1) 对齐；models 响应更小，取 1 MiB）。
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes+1))
	if rerr != nil {
		return nil, false, 0, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxModelsBodyBytes {
		return nil, false, 0, fmt.Errorf("models 响应超过 %d 字节上限", maxModelsBodyBytes)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, 0, fmt.Errorf("parse response: %w", err)
	}
	ids := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		ids = append(ids, m.ID)
	}
	return ids, false, 0, nil
}

// ListAllModels 聚合多个 provider 的可用模型，以 config.ModelRef 切片返回（provider/model 分离）。
// 并发请求各 provider 的 ModelsURL（最多 8 路并发）；静态 models（无 ModelsURL）直接返回配置，不 GET。
// 单个 provider 失败时记录警告但继续其他，最终返回首个错误（若有）。
// keyFor 按 provider 返回最终 API key；httpClient 非 nil 时复用其 transport/timeout。
// 调用方须保证 providers 已校验（名称唯一、URL 合法）。
func ListAllModels(ctx context.Context, providers []config.ProviderConfig, keyFor func(config.ProviderConfig) string, httpClient *http.Client, logger *slog.Logger) ([]config.ModelRef, error) {
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	// 按 provider 名称收集结果，最后按输入顺序拼接，保证输出稳定。
	results := make(map[string][]config.ModelRef, len(providers))

	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)

	for _, p := range providers {
		wg.Add(1)
		sem <- struct{}{}
		go func(p config.ProviderConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			var ids []string
			var err error
			if p.ModelsURL == "" {
				if len(p.Models) == 0 {
					err = fmt.Errorf("provider %q 无 models_url 且静态 models 为空", p.Name)
				} else {
					ids = make([]string, 0, len(p.Models))
					for _, mm := range p.Models {
						ids = append(ids, mm.Name)
					}
				}
			} else {
				llm, e := NewChatClient(keyFor(p), p.ChatURL, p.ModelsURL, httpClient, logger, p.Headers)
				if e != nil {
					err = e
				} else {
					ids, err = llm.ListModels(ctx)
				}
			}
			if err != nil {
				if logger != nil {
					logger.Warn("list models failed", "provider", p.Name, "error", err)
				}
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("provider %q: %w", p.Name, err)
				}
				mu.Unlock()
				return
			}
			paired := make([]config.ModelRef, 0, len(ids))
			for _, id := range ids {
				paired = append(paired, config.ModelRef{Provider: p.Name, Model: id})
			}
			mu.Lock()
			results[p.Name] = paired
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	all := make([]config.ModelRef, 0)
	for _, p := range providers {
		all = append(all, results[p.Name]...)
	}
	return all, firstErr
}
