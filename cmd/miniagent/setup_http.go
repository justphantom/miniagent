package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/miniagent/event"
	"github.com/justphantom/miniagent/internal/provider/anthropic"
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

// listAllModels resolves the key per provider (provider.Key > $MINIAGENT_API_KEY) and reuses a unified
// transport/timeout to aggregate the model list.
func listAllModels(ctx context.Context, providers []config.ProviderConfig, httpTimeout time.Duration, logger *slog.Logger) ([]openai.ProviderModel, error) {
	keyFor := func(p config.ProviderConfig) string {
		if p.Key != "" {
			return p.Key
		}
		return os.Getenv("MINIAGENT_API_KEY")
	}
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	httpClient := newHTTPClient(httpTimeout, newHTTPTransport())
	return openai.ListAllModels(ctx, providers, keyFor, httpClient, logger)
}

// FetchModelLimits GETs the provider's models endpoint once and returns the limits reported for
// modelID (non-standard context_window/max_output_tokens extensions; nil fields when the endpoint
// omits them or the model is absent from the list). Only kind=openai providers are supported —
// the Anthropic /v1/models endpoint reports ids only. Best-effort: errors are the caller's to warn
// and continue on (a down models endpoint must not block the run; the fallback is config-only limits).
func FetchModelLimits(ctx context.Context, p config.ProviderConfig, modelID, apiKey string, httpTimeout time.Duration, logger *slog.Logger) (config.ModelLimits, error) {
	if p.ModelsURL == "" || providerKind(p.Kind) != "openai" {
		return config.ModelLimits{}, nil
	}
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	llm, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers)
	if err != nil {
		return config.ModelLimits{}, err
	}
	models, err := llm.ListModels(ctx)
	if err != nil {
		return config.ModelLimits{}, err
	}
	for _, m := range models {
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

// providerKind normalizes the Kind field: empty defaults to "openai" (backward-compatible with configs
// written before the Kind field existed).
func providerKind(kind string) string {
	if kind == "" {
		return "openai"
	}
	return kind
}

// buildLLM constructs the main provider's LLM by ProviderConfig.Kind: "anthropic" routes to the Anthropic
// Messages API provider, anything else ("" / "openai") to the OpenAI Chat Completions provider. The returned
// LLM also satisfies miniagent.Doer (the compaction fallback uses it directly). httpTimeout<=0 = default 120s.
func buildLLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.LLM {
	switch providerKind(p.Kind) {
	case "anthropic":
		return buildAnthropicLLM(apiKey, p, logger, httpTimeout)
	default:
		return buildOpenAILLM(apiKey, p, logger, httpTimeout)
	}
}

// buildOpenAILLM constructs the OpenAI Chat Completions provider: a ChatClient (overall Timeout, non-streaming
// + models) and a StreamClient (no Timeout, streaming), both sharing one *http.Transport. Chat's httpTimeout
// is a fallback against a single call hanging; stream has no Timeout so the body read is not cut.
func buildOpenAILLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.LLM {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	transport := newHTTPTransport()
	chatClient := newHTTPClient(httpTimeout, transport)
	streamClient := &http.Client{Transport: transport}
	chat, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, chatClient, logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream, err := openai.NewStreamClient(apiKey, p.ChatURL, streamClient, logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream.StreamAllowUnterminated = p.StreamAllowUnterminated != nil && *p.StreamAllowUnterminated
	return &openai.Provider{Chat: chat, Stream: stream}
}

// buildAnthropicLLM constructs the Anthropic Messages API provider. cache toggles prompt-caching breakpoints
// (nil/auto or true → enabled; false → kill-switch). Client (non-streaming) and StreamClient (streaming) share
// one *http.Transport with the same timeout split as the openai path.
func buildAnthropicLLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.LLM {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	transport := newHTTPTransport()
	chatClient := newHTTPClient(httpTimeout, transport)
	streamClient := &http.Client{Transport: transport}
	cache := p.Cache == nil || *p.Cache
	chat, err := anthropic.NewClient(apiKey, p.ChatURL, p.ModelsURL, chatClient, logger, p.Headers, cache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream, err := anthropic.NewStreamClient(apiKey, p.ChatURL, streamClient, logger, p.Headers, cache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream.StreamAllowUnterminated = p.StreamAllowUnterminated != nil && *p.StreamAllowUnterminated
	return &anthropic.Provider{Chat: chat, Stream: stream}
}

// buildDoer constructs a non-streaming Doer for the specified provider (used by scenarios that only need Do,
// such as cross-provider compaction summarization). Routes by Kind the same way as buildLLM.
func buildDoer(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.Doer {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	switch providerKind(p.Kind) {
	case "anthropic":
		cache := p.Cache == nil || *p.Cache
		chat, err := anthropic.NewClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers, cache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
			os.Exit(1)
		}
		return chat
	default:
		chat, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
			os.Exit(1)
		}
		return chat
	}
}

// newHTTPTransport returns the reused *http.Transport, configuring proxy, dial, TLS, and response-header timeouts.
func newHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		// Was 30s: slow endpoints (e.g. agnes) often got requests cut here, causing long-input scenarios like compaction summarization to fail.
		// Relaxed to 300s; the side effect is that any provider's slow request hangs longer (takes effect together with http.Client.Timeout).
		// Note: 300s actually only takes effect on the stream path (stream has no Client.Timeout); chat/compaction response-header
		// waiting is capped by each Client.Timeout(120s), so 300s is a redundant upper bound for them (only relaxes the old 30s cutoff).
		ResponseHeaderTimeout: 300 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// newHTTPClient returns a *http.Client with the specified overall timeout and transport.
func newHTTPClient(timeout time.Duration, transport *http.Transport) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}
