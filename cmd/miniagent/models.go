package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/provider/anthropic"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// modelSource is one provider endpoint to list models from: the decoupled view of a provider
// entry (config.ProviderConfig maps onto this at the cmd layer) so the provider packages do not
// depend on the CLI config layer. Kind ""/"openai" → OpenAI-compatible client; "anthropic" → Anthropic client.
type modelSource struct {
	Name         string
	Kind         string
	ChatURL      string
	ModelsURL    string
	Headers      map[string]string
	StaticModels []string
}

// providerModel pairs a provider name with one model entry.
type providerModel struct {
	Provider string
	Model    string
	Limits   modelLimits
}

// modelLimits mirrors the capability limit fields reported by models endpoints.
type modelLimits struct {
	ContextWindow   *int
	MaxOutputTokens *int
}

// modelSources maps config providers onto the cmd-layer decoupled view.
func modelSources(providers []config.ProviderConfig) []modelSource {
	out := make([]modelSource, 0, len(providers))
	for _, p := range providers {
		ms := modelSource{Name: p.Name, Kind: p.Kind, ChatURL: p.ChatURL, ModelsURL: p.ModelsURL, Headers: p.Headers}
		if p.ModelsURL == "" {
			ms.StaticModels = make([]string, 0, len(p.Models))
			for _, m := range p.Models {
				ms.StaticModels = append(ms.StaticModels, m.Name)
			}
		}
		out = append(out, ms)
	}
	return out
}

// listAllModels aggregates the available models across multiple providers and returns them as a
// slice of providerModel (provider/model kept separate, plus optional capability limits reported
// by the endpoint). It requests each provider's ModelsURL concurrently (at most 8 in flight);
// static models (no ModelsURL) return the config directly without a GET. kind=anthropic providers
// dispatch to the Anthropic /v1/models endpoint. A single provider failure is logged as a warning
// but the rest continue; the first error (if any) is returned at the end.
// keyFor returns the final API key per provider; when httpClient is non-nil its transport/timeout is reused.
func listAllModels(ctx context.Context, providers []config.ProviderConfig, httpTimeout time.Duration, logger *slog.Logger) ([]providerModel, error) {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	httpClient := newHTTPClient(httpTimeout, newHTTPTransport())
	keyFor := func(p modelSource) string {
		if pKey := providerKeyByName(providers, p.Name); pKey != "" {
			return pKey
		}
		return os.Getenv("MINIAGENT_API_KEY")
	}
	srcs := modelSources(providers)

	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	results := make(map[string][]providerModel, len(srcs))

	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)

	for _, p := range srcs {
		wg.Add(1)
		sem <- struct{}{}
		go func(p modelSource) {
			defer wg.Done()
			defer func() { <-sem }()
			var models []openai.ModelInfo
			var err error
			if p.ModelsURL == "" {
				if len(p.StaticModels) == 0 {
					err = fmt.Errorf("provider %q has no models_url and its static models list is empty", p.Name)
				} else {
					models = make([]openai.ModelInfo, 0, len(p.StaticModels))
					for _, id := range p.StaticModels {
						models = append(models, openai.ModelInfo{ID: id})
					}
				}
			} else if p.Kind == "anthropic" {
				llm, e := anthropic.NewClient(keyFor(p), p.ChatURL, p.ModelsURL, httpClient, logger, p.Headers, false)
				if e != nil {
					err = e
				} else {
					anthModels, e2 := llm.ListModels(ctx)
					err = e2
					models = make([]openai.ModelInfo, 0, len(anthModels))
					for _, m := range anthModels {
						models = append(models, openai.ModelInfo{ID: m.ID, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens})
					}
				}
			} else {
				llm, e := openai.NewChatClient(keyFor(p), p.ChatURL, p.ModelsURL, httpClient, logger, p.Headers)
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
			paired := make([]providerModel, 0, len(models))
			for _, m := range models {
				paired = append(paired, providerModel{Provider: p.Name, Model: m.ID, Limits: modelLimits{ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens}})
			}
			mu.Lock()
			results[p.Name] = paired
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	all := make([]providerModel, 0, len(srcs))
	for _, p := range srcs {
		all = append(all, results[p.Name]...)
	}
	return all, firstErr
}
