package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/httpretry"
)

const maxModelsBodyBytes = 1 << 20 // 1 MiB; the models list is far smaller than the chat body, cap prevents OOM

// ModelInfo is one model entry returned by ListModels: the id plus optional capability limits
// (context_window/max_output_tokens) reported by the endpoint. The limit fields are non-standard
// extensions absent on official OpenAI /v1/models; pointers stay nil when the endpoint omits them.
type ModelInfo struct {
	ID              string
	ContextWindow   *int
	MaxOutputTokens *int
}

// ListModels issues GET ModelsURL and returns the model list with optional capability limits.
// Reuses ChatClient.modelsEndpoint/auth. Retries 429/5xx and network errors automatically up to
// maxRetries times with the same backoff policy as Do.
func (c *ChatClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
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
// retryAfter is parsed from the Retry-After header (-1 = absent) so ListModels retry backoff aligns
// with ChatClient.Do (previously it was always -1, ignoring upstream backpressure).
func (c *ChatClient) listModelsOnce(ctx context.Context, client *http.Client, u *url.URL) ([]ModelInfo, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("User-Agent", miniagent.UserAgent())
	// Inject provider custom headers; skip Authorization/Content-Type to avoid clobbering auth (same as ChatClient.doOnce).
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
		// Drain the body so the connection can be reused via keepalive (a retry may reuse the same
		// connection); cap at maxModelsBodyBytes to stop a malicious/abnormal endpoint from slow-
		// trickling each retry out to the client timeout (aligned with the 200 path's LimitReader).
		// The response body is not echoed — a malicious/debug proxy may echo the URL/Authorization in
		// the error body, and the error flows through -list-models stdout leaking the key (same as
		// doOnce, only the status code is surfaced).
		retryAfter := httpretry.ParseRetryAfter(resp.Header)
		if _, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxModelsBodyBytes+1)); readErr != nil {
			return nil, shouldRetryStatus(resp.StatusCode), retryAfter, fmt.Errorf("llm returned %d: read body: %w", resp.StatusCode, readErr)
		}
		return nil, shouldRetryStatus(resp.StatusCode), retryAfter, fmt.Errorf("llm returned %d", resp.StatusCode)
	}
	// 200 path caps to prevent OOM (aligned with doOnce's LimitReader(maxChatBodyBytes+1); models
	// responses are smaller, so 1 MiB is used).
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes+1))
	if rerr != nil {
		return nil, false, 0, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxModelsBodyBytes {
		return nil, false, 0, fmt.Errorf("models response exceeded %d byte limit", maxModelsBodyBytes)
	}
	var v struct {
		Data []struct {
			ID              string `json:"id"`
			ContextWindow   *int   `json:"context_window,omitempty"`
			MaxOutputTokens *int   `json:"max_output_tokens,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, 0, fmt.Errorf("parse response: %w", err)
	}
	models := make([]ModelInfo, 0, len(v.Data))
	for _, m := range v.Data {
		models = append(models, ModelInfo{ID: m.ID, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens})
	}
	return models, false, 0, nil
}
