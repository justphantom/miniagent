package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/provider/anthropic"
	"github.com/justphantom/miniagent/internal/provider/openai"
	"github.com/justphantom/miniagent/internal/provider/responses"
)

// providerKind normalizes the Kind field: empty defaults to "openai" (backward-compatible with configs
// written before the Kind field existed).
func providerKind(kind string) string {
	if kind == "" {
		return "openai"
	}
	return kind
}

// buildLLM constructs the main provider's LLM by ProviderConfig.Kind. The returned LLM also satisfies
// miniagent.Doer (the compaction fallback uses it directly). httpTimeout<=0 = default 120s.
func buildLLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.LLM {
	switch providerKind(p.Kind) {
	case "anthropic":
		return buildAnthropicLLM(apiKey, p, logger, httpTimeout)
	case "responses":
		return buildResponsesLLM(apiKey, p, logger, httpTimeout)
	default:
		return buildOpenAILLM(apiKey, p, logger, httpTimeout)
	}
}

// buildResponsesLLM constructs the OpenAI Responses provider with the same chat/stream timeout split.
func buildResponsesLLM(apiKey string, p config.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) miniagent.LLM {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	transport := newHTTPTransport()
	chatClient := newHTTPClient(httpTimeout, transport)
	streamClient := &http.Client{Transport: transport}
	chat, err := responses.NewClient(apiKey, p.ChatURL, chatClient, logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream, err := responses.NewStreamClient(apiKey, p.ChatURL, streamClient, logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream.StreamAllowUnterminated = p.StreamAllowUnterminated != nil && *p.StreamAllowUnterminated
	return &responses.Provider{Chat: chat, Stream: stream}
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
	case "responses":
		chat, err := responses.NewClient(apiKey, p.ChatURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers)
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
