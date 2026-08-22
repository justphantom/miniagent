package main

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/provider/openai"
)

// buildLLM constructs the main provider's LLM (OpenAI Chat Completions). The returned LLM also satisfies
// miniagent.Doer (the compaction fallback uses it directly). httpTimeout<=0 = default 120s.
func buildLLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration, cache *transportCache) (miniagent.LLM, error) {
	return buildOpenAILLM(apiKey, p, logger, httpTimeout, cache)
}

// transportCache hands out one *http.Transport per provider name, so a long-running -serve process
// reuses connections (no per-turn TCP+TLS handshake) instead of leaking a new keep-alive pool per turn.
// config hot-reload does not exist: the provider set is fixed after startup, so keying by name is stable.
type transportCache struct {
	mu sync.Mutex
	m  map[string]*http.Transport
}

func (c *transportCache) get(name string) *http.Transport {
	if c == nil {
		return newHTTPTransport()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.m[name]; ok {
		return t
	}
	t := newHTTPTransport()
	if c.m == nil {
		c.m = make(map[string]*http.Transport)
	}
	c.m[name] = t
	return t
}

// buildOpenAILLM constructs the OpenAI Chat Completions provider: a ChatClient (overall Timeout, non-streaming
// + models) and a StreamClient (no Timeout, streaming), both sharing one *http.Transport. Chat's httpTimeout
// is a fallback against a single call hanging; stream has no Timeout so the body read is not cut.
func buildOpenAILLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration, cache *transportCache) (miniagent.LLM, error) {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	transport := cache.get(p.Name)
	chatClient := newHTTPClient(httpTimeout, transport)
	streamClient := &http.Client{Transport: transport}
	chat, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, chatClient, logger, p.Headers)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint url: %w", err)
	}
	stream, err := openai.NewStreamClient(apiKey, p.ChatURL, streamClient, logger, p.Headers)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint url: %w", err)
	}
	stream.StreamAllowUnterminated = p.StreamAllowUnterminated != nil && *p.StreamAllowUnterminated
	return &openai.Provider{Chat: chat, Stream: stream}, nil
}

// buildDoer constructs a non-streaming Doer for the specified provider (used by scenarios that only need Do,
// such as cross-provider compaction summarization).
func buildDoer(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration, cache *transportCache) (miniagent.Doer, error) {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	chat, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, cache.get(p.Name)), logger, p.Headers)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint url: %w", err)
	}
	return chat, nil
}

// buildRuntimeClients constructs the main provider's LLM and (when compaction crosses providers) the
// summarization Doer. compChat is left nil when compaction uses the same provider (compaction falls back to
// llm, which satisfies miniagent.Doer). Errors are returned (not os.Exit) so the long-running -serve process
// can answer the request with 500 instead of dying.
func buildRuntimeClients(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
	llm, err := buildLLM(apiKey, resolved.Provider, logger, httpTimeoutOf(resolved), cache)
	if err != nil {
		return nil, nil, err
	}
	if resolved.CompactionProvider.Name != resolved.Provider.Name {
		key := resolveFinalKey(resolved.CompactionProvider.Key)
		if key == "" {
			return nil, nil, errors.New("compaction provider API key missing (provider.key / $MINIAGENT_API_KEY)")
		}
		warnProviderInsecureURLs(resolved.CompactionProvider)
		comp, err := buildDoer(key, resolved.CompactionProvider, logger, httpTimeoutOf(resolved), cache)
		if err != nil {
			return nil, nil, err
		}
		return llm, comp, nil
	}
	return llm, nil, nil
}
