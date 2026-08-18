package main

import (
	"net"
	"net/http"
	"time"
)

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
