package config

import (
	"fmt"
	"net/url"
)

// ValidateURL parses and validates raw as a legal http(s) URL (with scheme+host).
// Shared by the core config validation and the provider implementation (internal/provider/openai), to avoid divergence.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url %q failed to parse: %w (expected http(s)://host[:port][/path])", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("url %q is missing scheme or host (expected http(s)://host[:port])", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url %q scheme %q is not supported (http/https only)", raw, u.Scheme)
	}
	// Reject embedded userinfo (https://key@host): credentials embedded in the URL get logged in error bodies, and the Go transport may also
	// send them as Basic Auth. The API key must be injected via provider.key / $MINIAGENT_API_KEY, never in the URL.
	if u.User != nil {
		return nil, fmt.Errorf("url %q contains userinfo (user:pass@host) — forbidden, to prevent credentials embedded in the URL from being logged or leaked", raw)
	}
	return u, nil
}
