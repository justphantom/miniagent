package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// buildLLM constructs the main provider's LLM (OpenAI Chat Completions). The returned LLM also satisfies
// miniagent.Doer (the compaction fallback uses it directly). httpTimeout<=0 = default 120s.
func buildLLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.LLM {
	return buildOpenAILLM(apiKey, p, logger, httpTimeout)
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

// buildDoer constructs a non-streaming Doer for the specified provider (used by scenarios that only need Do,
// such as cross-provider compaction summarization).
func buildDoer(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.Doer {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	chat, err := openai.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	return chat
}

// buildRuntimeClients constructs the main provider's LLM and (when compaction crosses providers) the
// summarization Doer. compChat is left nil when compaction uses the same provider (compaction falls back to
// llm, which satisfies miniagent.Doer). os.Exit on missing key or invalid endpoint.
func buildRuntimeClients(resolved *config.Resolved, apiKey string, logger *slog.Logger) (miniagent.LLM, miniagent.Doer) {
	llm := buildLLM(apiKey, resolved.Provider, logger, httpTimeoutOf(resolved))
	if resolved.CompactionProvider.Name != resolved.Provider.Name {
		key := resolveFinalKey(resolved.CompactionProvider.Key)
		if key == "" {
			fmt.Fprintf(os.Stderr, "miniagent: compaction provider API key missing (provider.key / $MINIAGENT_API_KEY)\n")
			os.Exit(1)
		}
		warnProviderInsecureURLs(resolved.CompactionProvider)
		return llm, buildDoer(key, resolved.CompactionProvider, logger, httpTimeoutOf(resolved))
	}
	return llm, nil
}
