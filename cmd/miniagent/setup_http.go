package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/miniagent/event"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// warnProviderInsecureURLs warns about plaintext key transmission for http (non-loopback) URLs used by the provider.
func warnProviderInsecureURLs(p config.ProviderConfig) {
	warnInsecureURL(p.ChatURL)
	if p.ModelsURL != "" {
		warnInsecureURL(p.ModelsURL)
	}
}

func warnProvidersInsecureURLs(providers []config.ProviderConfig) {
	for _, p := range providers {
		warnProviderInsecureURLs(p)
	}
}

// httpTimeoutFromConfig parses run.http_timeout from config; returns 0 when not configured. Reuses config.ParseDuration
// to unify duration validation semantics and error format (the list-models path bypasses Resolve, so it is parsed separately here).
func httpTimeoutFromConfig(cfg *config.Config) (time.Duration, error) {
	d, err := config.ParseDuration(cfg.Run.HTTPTimeout, "run.http_timeout")
	if err != nil || d == nil {
		return 0, err
	}
	return *d, nil
}

// providerKeyByName returns provider.Key by name from the config slice (modelSource deliberately
// carries no key material; the key is resolved at the cmd layer per call).
func providerKeyByName(providers []config.ProviderConfig, name string) string {
	for _, p := range providers {
		if p.Name == name {
			return p.Key
		}
	}
	return ""
}

// FetchModelLimits GETs the provider's models endpoint once and returns the limits reported for
// modelID (non-standard context_window/max_output_tokens extensions; nil fields when the endpoint
// omits them or the model is absent from the list). Best-effort: errors are the caller's to warn and
// continue on (a down models endpoint must not block the run; the fallback is config-only limits).
func FetchModelLimits(ctx context.Context, p config.ProviderConfig, modelID, apiKey string, httpTimeout time.Duration, logger *slog.Logger) (config.ModelLimits, error) {
	if p.ModelsURL == "" {
		return config.ModelLimits{}, nil
	}
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	llm, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers)
	if err != nil {
		return config.ModelLimits{}, err
	}
	om, err := llm.ListModels(ctx)
	if err != nil {
		return config.ModelLimits{}, err
	}
	for _, m := range om {
		if m.ID == modelID {
			return config.ModelLimits{ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens}, nil
		}
	}
	return config.ModelLimits{}, nil
}

// runListModels implements the -list-models early-exit path: outputs NDJSON model events line by line (provider/model
// as separate fields). When providerFilter is non-empty only that provider is listed. Partial failure: successful entries are still output, exit code 1.
func runListModels(ctx context.Context, cfg *config.Config, providerFilter string, logger *slog.Logger) {
	listHTTPTimeout, err := httpTimeoutFromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: config: %v\n", err)
		os.Exit(1)
	}
	providers := cfg.Providers
	if providerFilter != "" {
		p, err := config.FindProvider(cfg, providerFilter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
			os.Exit(1)
		}
		providers = []config.ProviderConfig{p}
	}
	warnProvidersInsecureURLs(providers)
	models, err := listAllModels(ctx, providers, listHTTPTimeout, logger)
	for _, m := range models {
		if emitErr := event.EmitModel(os.Stdout, m.Provider, m.Model); emitErr != nil {
			fmt.Fprintf(os.Stderr, "miniagent: emit model: %v\n", emitErr)
			os.Exit(1)
		}
	}
	if err != nil {
		// Signal cancellation (SIGINT/SIGTERM) takes code 130 to exit cleanly, no error — consistent with the main Run path (main.go).
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "miniagent: list models: %v\n", err)
		os.Exit(1)
	}
}

// warnInsecureURL: when http (non-loopback) the API key is sent in plaintext over the wire; warn on stderr. Does not forcibly reject.
func warnInsecureURL(rawURL string) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	fmt.Fprintf(os.Stderr, "miniagent: warning: endpoint %s uses plain http, API key sent unencrypted\n", u.Redacted())
}
