package responses

import (
	"context"

	"github.com/justphantom/miniagent/internal/miniagent"
)

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
