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

	"github.com/justphantom/miniagent/internal/provider/anthropic"
	"github.com/justphantom/miniagent/internal/provider/httpretry"
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

// ModelSource is one provider endpoint to list models from: the decoupled view of a provider
// entry (config.ProviderConfig maps onto this at the cmd layer) so the provider package does not
// depend on the CLI config layer. Kind ""/"openai" is the OpenAI-compatible protocol; "anthropic"
// dispatches to the Anthropic /v1/models client.
type ModelSource struct {
	Name      string
	Kind      string
	ChatURL   string
	ModelsURL string
	Headers   map[string]string
	// StaticModels lists model ids without a ModelsURL (returned without a GET).
	StaticModels []string
}

// ProviderModel pairs a provider name with one model entry from that provider's listing.
type ProviderModel struct {
	Provider string
	Model    string
	Limits   ModelLimits
}

// ModelLimits mirrors the capability limit fields reported by models endpoints
// (non-standard context_window/max_output_tokens extensions); nil when absent.
type ModelLimits struct {
	ContextWindow   *int
	MaxOutputTokens *int
}

// ListAllModels aggregates the available models across multiple providers and returns them as a
// slice of ProviderModel (provider/model kept separate, plus optional capability limits reported
// by the endpoint). It requests each provider's ModelsURL concurrently (at most 8 in flight);
// static models (no ModelsURL) return the config directly without a GET. kind=anthropic providers
// dispatch to the Anthropic /v1/models endpoint (same auth headers as chat); that endpoint reports
// ids only, so Limits stay nil for those entries. A single provider failure
// is logged as a warning but the rest continue; the first error (if any) is returned at the end.
// keyFor returns the final API key per provider; when httpClient is non-nil its transport/timeout
// is reused. The caller must ensure providers are already validated (unique names, valid URLs).
func ListAllModels(ctx context.Context, providers []ModelSource, keyFor func(ModelSource) string, httpClient *http.Client, logger *slog.Logger) ([]ProviderModel, error) {
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	// Collect results keyed by provider name, then concatenate in input order for stable output.
	results := make(map[string][]ProviderModel, len(providers))

	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)

	for _, p := range providers {
		wg.Add(1)
		sem <- struct{}{}
		go func(p ModelSource) {
			defer wg.Done()
			defer func() { <-sem }()
			var models []ModelInfo
			var err error
			if p.ModelsURL == "" {
				if len(p.StaticModels) == 0 {
					err = fmt.Errorf("provider %q has no models_url and its static models list is empty", p.Name)
				} else {
					models = make([]ModelInfo, 0, len(p.StaticModels))
					for _, id := range p.StaticModels {
						models = append(models, ModelInfo{ID: id})
					}
				}
			} else if p.Kind == "anthropic" {
				// Anthropic official /v1/models reports ids only; proxy upstreams may add the same
				// context_window/max_output_tokens extensions as openai, so limits flow through when present.
				llm, e := anthropic.NewClient(keyFor(p), p.ChatURL, p.ModelsURL, httpClient, logger, p.Headers, false)
				if e != nil {
					err = e
				} else {
					var anthModels []anthropic.ModelInfo
					anthModels, err = llm.ListModels(ctx)
					models = make([]ModelInfo, 0, len(anthModels))
					for _, m := range anthModels {
						models = append(models, ModelInfo{ID: m.ID, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens})
					}
				}
			} else {
				llm, e := NewChatClient(keyFor(p), p.ChatURL, p.ModelsURL, httpClient, logger, p.Headers)
				if e != nil {
					err = e
				} else {
					models, err = llm.ListModels(ctx)
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
			paired := make([]ProviderModel, 0, len(models))
			for _, m := range models {
				paired = append(paired, ProviderModel{Provider: p.Name, Model: m.ID, Limits: ModelLimits{ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens}})
			}
			mu.Lock()
			results[p.Name] = paired
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	all := make([]ProviderModel, 0)
	for _, p := range providers {
		all = append(all, results[p.Name]...)
	}
	return all, firstErr
}
