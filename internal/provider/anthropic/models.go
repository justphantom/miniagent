package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/justphantom/miniagent/internal/provider/httpretry"
)

const maxModelsBodyBytes = 1 << 20 // 1 MiB; mirrors openai — the models list is far smaller than the chat body

// ModelInfo is one model entry returned by ListModels. The Anthropic /v1/models endpoint reports only
// id/display_name/created_at/type — no context_window/max_output_tokens — so unlike openai.ModelInfo
// this carries no capability limits (display_name is intentionally not projected: the CLI outputs id only).
type ModelInfo struct {
	ID string
}

// ListModels issues GET ModelsURL?limit=1000 and returns the first page of models. Mirrors
// openai.ChatClient.ListModels retry policy (429/5xx and network errors retry up to MaxRetries with
// backoff / Retry-After). Pagination is deliberately not followed (has_more ignored): the upstream
// page cap is 1000 and the real model count is far below it, so one page suffices.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	client, u, err := c.modelsEndpoint(30 * time.Second)
	if err != nil {
		return nil, err
	}
	backoff := httpretry.RetryBaseDelay
	for attempt := 0; attempt <= httpretry.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		models, retryable, retryAfter, err := c.listModelsOnce(ctx, client, u)
		if err == nil {
			return models, nil
		}
		if !retryable || attempt == httpretry.MaxRetries {
			if attempt > 0 {
				return nil, fmt.Errorf("after %d retries: %w", attempt, err)
			}
			return nil, err
		}
		delay := httpretry.CapRetryDelay(backoff, retryAfter)
		if c.Logger != nil {
			c.Logger.Warn("list models failed, retrying", "failed_attempt", attempt+1, "delay_ms", delay.Milliseconds(), "error", err)
		}
		if waitErr := httpretry.SleepCtx(ctx, delay); waitErr != nil {
			return nil, waitErr
		}
		backoff *= 2
	}
	return nil, errors.New("list models retry loop exited unexpectedly")
}

// listModelsOnce performs a single GET /models and returns (models, retryable, retryAfter, error).
// Mirrors openai listModelsOnce: error bodies are drained (keepalive reuse) but never echoed — a
// proxy may reflect the URL/key into the body and the error reaches -list-models stdout.
func (c *Client) listModelsOnce(ctx context.Context, client *http.Client, u *url.URL) ([]ModelInfo, bool, time.Duration, error) {
	u2 := *u
	q := u2.Query()
	q.Set("limit", "1000")
	u2.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u2.String(), nil)
	if err != nil {
		return nil, false, 0, fmt.Errorf("build request: %w", err)
	}
	c.setAuthHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, -1, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		retryAfter := httpretry.ParseRetryAfter(resp.Header)
		if _, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxModelsBodyBytes+1)); readErr != nil {
			return nil, shouldRetryStatus(resp.StatusCode), retryAfter, fmt.Errorf("llm returned %d: read body: %w", resp.StatusCode, readErr)
		}
		return nil, shouldRetryStatus(resp.StatusCode), retryAfter, fmt.Errorf("llm returned %d", resp.StatusCode)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes+1))
	if rerr != nil {
		return nil, false, 0, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxModelsBodyBytes {
		return nil, false, 0, fmt.Errorf("models response exceeded %d byte limit", maxModelsBodyBytes)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, 0, fmt.Errorf("parse response: %w", err)
	}
	models := make([]ModelInfo, 0, len(v.Data))
	for _, m := range v.Data {
		models = append(models, ModelInfo{ID: m.ID})
	}
	if len(models) == 0 {
		return nil, false, 0, errors.New("model list is empty")
	}
	return models, false, 0, nil
}
