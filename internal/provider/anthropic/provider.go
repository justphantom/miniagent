// Package anthropic is the Anthropic Messages API provider implementation: request serialization
// (wire + wire_blocks), non-streaming client (Client), streaming client (StreamClient), SSE parsing
// (sse), and retry/backoff (retry). It is an alternative implementation of the core miniagent.LLM
// interface — the core loop is decoupled from vendors through it (the same seam as the openai provider),
// selected at the cmd layer by config ProviderConfig.Kind == "anthropic". The wire layer performs a
// lossy projection from the flat core domain types to Anthropic's typed content-block model.
package anthropic

import (
	"context"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// Provider is the Anthropic Messages API implementation of miniagent.LLM: it composes Client (Do,
// non-streaming) and StreamClient (DoStream, streaming), mirroring openai.Provider. The cmd wires it up to
// feed Run when a provider's kind is "anthropic".
type Provider struct {
	Chat   *Client
	Stream *StreamClient
}

func (p *Provider) Do(ctx context.Context, req miniagent.Request) (miniagent.Response, error) {
	return p.Chat.Do(ctx, req)
}

func (p *Provider) DoStream(ctx context.Context, req miniagent.Request, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	return p.Stream.DoStream(ctx, req, onDelta)
}
