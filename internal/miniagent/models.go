package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"log/slog"
)

// ListModels 调 GET ModelsURL，返回 id 列表。复用 ChatClient.modelsEndpoint/鉴权。
func (c *ChatClient) ListModels(ctx context.Context) ([]string, error) {
	client, u, err := c.modelsEndpoint(30 * time.Second)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := truncate(string(raw), 500, "…")
		if readErr != nil {
			return nil, fmt.Errorf("llm returned %d: read body: %w (partial: %s)", resp.StatusCode, readErr, body)
		}
		return nil, fmt.Errorf("llm returned %d: %s", resp.StatusCode, body)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	ids := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// ListAvailableModels 按 provider 解析可用模型：ModelsURL 空则直接返回静态 Models（不 GET），
// 静态也空则报错；否则经 llm GET ModelsURL（审查 v3 #5）。调用方须保证 llm 与 p 同源。
func ListAvailableModels(ctx context.Context, llm *ChatClient, p ProviderConfig) ([]string, error) {
	if p.ModelsURL == "" {
		if len(p.Models) == 0 {
			return nil, fmt.Errorf("provider %q 无 models_url 且静态 models 为空", p.Name)
		}
		return append([]string(nil), p.Models...), nil
	}
	return llm.ListModels(ctx)
}

// ListAllModels 聚合多个 provider 的可用模型，输出 "provider/model_id" 格式。
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
				llm, e := NewChatClient(keyFor(p), p.ChatURL, p.ModelsURL, httpClient, logger)
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
