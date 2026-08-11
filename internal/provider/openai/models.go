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
	"github.com/justphantom/miniagent/internal/provider/httpretry"
)

const maxModelsBodyBytes = 1 << 20 // 1 MiB; the models list is far smaller than the chat body, cap prevents OOM

// ListModels issues GET ModelsURL and returns the id list. Reuses ChatClient.modelsEndpoint/auth.
// Retries 429/5xx and network errors automatically up to maxRetries times with the same backoff policy as Do.
func (c *ChatClient) ListModels(ctx context.Context) ([]string, error) {
	client, u, err := c.modelsEndpoint(30 * time.Second)
	if err != nil {
		return nil, err
	}
	backoff := httpretry.RetryBaseDelay
	for attempt := 0; attempt <= httpretry.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids, retryable, retryAfter, err := c.listModelsOnce(ctx, client, u)
		if err == nil {
			return ids, nil
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

// listModelsOnce performs a single GET /models and returns (ids, retryable, retryAfter, error).
// retryAfter is parsed from the Retry-After header (-1 = absent) so ListModels retry backoff aligns
// with ChatClient.Do (previously it was always -1, ignoring upstream backpressure).
func (c *ChatClient) listModelsOnce(ctx context.Context, client *http.Client, u *url.URL) ([]string, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
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

// ListAllModels aggregates the available models across multiple providers and returns them as a
// slice of config.ModelRef (provider/model kept separate). It requests each provider's ModelsURL
// concurrently (at most 8 in flight); static models (no ModelsURL) return the config directly
// without a GET. A single provider failure is logged as a warning but the rest continue; the first
// error (if any) is returned at the end. keyFor returns the final API key per provider; when
// httpClient is non-nil its transport/timeout is reused. The caller must ensure providers are
// already validated (unique names, valid URLs).
func ListAllModels(ctx context.Context, providers []config.ProviderConfig, keyFor func(config.ProviderConfig) string, httpClient *http.Client, logger *slog.Logger) ([]config.ModelRef, error) {
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	// Collect results keyed by provider name, then concatenate in input order for stable output.
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
					err = fmt.Errorf("provider %q has no models_url and its static models list is empty", p.Name)
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
