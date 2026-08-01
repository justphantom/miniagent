package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ListModels 调 GET {BaseURL}/v1/models，返回 id 列表。复用 endpoint/鉴权。
func (c *HTTPClient) ListModels(ctx context.Context) ([]string, error) {
	client, u, err := c.endpoint("/v1/models", 30*time.Second)
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
