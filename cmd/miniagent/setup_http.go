package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// warnProviderInsecureURLs 对 provider 使用的 http（非 loopback）URL 发出明文传 key 警告。
func warnProviderInsecureURLs(p miniagent.ProviderConfig) {
	warnInsecureURL(p.ChatURL)
	if p.ModelsURL != "" {
		warnInsecureURL(p.ModelsURL)
	}
}

func warnProvidersInsecureURLs(providers []miniagent.ProviderConfig) {
	for _, p := range providers {
		warnProviderInsecureURLs(p)
	}
}

// httpTimeoutFromConfig 解析 config 中的 run.http_timeout；未配置返回 0。
func httpTimeoutFromConfig(cfg *miniagent.Config) (time.Duration, error) {
	if cfg.Run.HTTPTimeout == nil {
		return 0, nil
	}
	d, err := time.ParseDuration(*cfg.Run.HTTPTimeout)
	if err != nil {
		return 0, fmt.Errorf("run.http_timeout %q: %w", *cfg.Run.HTTPTimeout, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("run.http_timeout %q: 负值不合法", *cfg.Run.HTTPTimeout)
	}
	return d, nil
}

// listAllModels 按 provider 解析 key（provider.Key > $MINIAGENT_API_KEY）并复用统一
// transport/timeout，聚合模型列表。
func listAllModels(ctx context.Context, providers []miniagent.ProviderConfig, httpTimeout time.Duration, logger *slog.Logger) ([]string, error) {
	keyFor := func(p miniagent.ProviderConfig) string {
		if p.Key != "" {
			return p.Key
		}
		return os.Getenv("MINIAGENT_API_KEY")
	}
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	httpClient := newHTTPClient(httpTimeout, newHTTPTransport())
	return miniagent.ListAllModels(ctx, providers, keyFor, httpClient, logger)
}

// warnInsecureURL：http（非 loopback）时 API key 明文上链，stderr 警告。不强制拒绝。
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

// buildLLM 构造 ChatClient（带总 Timeout，非流式 + models）与 StreamClient（无 Timeout，流式）。
// 两者共享同一 *http.Transport（代理/dial/TLS 超时）；chat 的 httpTimeout 兜底防单次调用挂死（#3），
// stream 无 Timeout 避免 body 读取被砍（P2-5/P1-A，#2）——P4 拆分后由各自 client 持有。
// httpTimeout<=0 用默认 120s。
func buildLLM(apiKey string, p miniagent.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) (*miniagent.ChatClient, *miniagent.StreamClient) {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	transport := newHTTPTransport()
	chatClient := newHTTPClient(httpTimeout, transport)
	streamClient := &http.Client{Transport: transport}
	chat, err := miniagent.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, chatClient, logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream, err := miniagent.NewStreamClient(apiKey, p.ChatURL, streamClient, logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	return chat, stream
}

// buildChatClient 为指定 provider 构造非流式 ChatClient（用于 compaction 等仅需 Do 的场景）。
func buildChatClient(apiKey string, p miniagent.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) *miniagent.ChatClient {
	if httpTimeout <= 0 {
		httpTimeout = 120 * time.Second
	}
	chat, err := miniagent.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, newHTTPClient(httpTimeout, newHTTPTransport()), logger, p.Headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	return chat
}

// newHTTPTransport 返回复用的 *http.Transport，配置代理、dial、TLS、响应头超时。
func newHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// newHTTPClient 返回带指定总 timeout 和 transport 的 *http.Client。
func newHTTPClient(timeout time.Duration, transport *http.Transport) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}
