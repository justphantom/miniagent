package miniagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"log/slog"
)

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
		ids, retryable, err := c.listModelsOnce(ctx, client, u)
		if err == nil {
			return ids, nil
		}
		if !retryable || attempt == maxRetries {
			if attempt > 0 {
				return nil, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return nil, err
		}
		delay := capRetryDelay(backoff, -1)
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

// listModelsOnce 单次 GET /models，返回 (ids, retryable, error)。
func (c *ChatClient) listModelsOnce(ctx context.Context, client *http.Client, u *url.URL) ([]string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	// 注入 provider 自定义头；Authorization 不在此覆盖。
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := truncate(string(raw), 500, "…")
		if readErr != nil {
			return nil, shouldRetryStatus(resp.StatusCode), fmt.Errorf("llm returned %d: read body: %w (partial: %s)", resp.StatusCode, readErr, body)
		}
		return nil, shouldRetryStatus(resp.StatusCode), fmt.Errorf("llm returned %d: %s", resp.StatusCode, body)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, false, fmt.Errorf("parse response: %w", err)
	}
	ids := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		ids = append(ids, m.ID)
	}
	return ids, false, nil
}

// ListAllModels 聚合多个 provider 的可用模型，统一输出 "provider/model_id" 格式。
// 并发请求各 provider 的 ModelsURL（最多 8 路并发）；静态 models（无 ModelsURL）直接返回配置，不 GET。
// 单个 provider 失败时记录警告但继续其他，最终返回首个错误（若有）。
// keyFor 按 provider 返回最终 API key；httpClient 非 nil 时复用其 transport/timeout。
// 调用方须保证 providers 已校验（名称唯一、URL 合法）。
func ListAllModels(ctx context.Context, providers []ProviderConfig, keyFor func(ProviderConfig) string, httpClient *http.Client, logger *slog.Logger) ([]string, error) {
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	// 按 provider 名称收集结果，最后按输入顺序拼接，保证输出稳定。
	results := make(map[string][]string, len(providers))

	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)

	for _, p := range providers {
		wg.Add(1)
		sem <- struct{}{}
		go func(p ProviderConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			var ids []string
			var err error
			if p.ModelsURL == "" {
				if len(p.Models) == 0 {
					err = fmt.Errorf("provider %q 无 models_url 且静态 models 为空", p.Name)
				} else {
					ids = append([]string(nil), p.Models...)
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
			paired := make([]string, 0, len(ids))
			for _, id := range ids {
				paired = append(paired, p.Name+"/"+id)
			}
			mu.Lock()
			results[p.Name] = paired
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	all := make([]string, 0)
	for _, p := range providers {
		all = append(all, results[p.Name]...)
	}
	return all, firstErr
}
