package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("llm returned %d: %s", resp.StatusCode, truncate(string(raw), 500, "…"))
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
