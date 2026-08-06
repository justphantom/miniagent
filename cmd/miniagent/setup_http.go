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
	"github.com/justphantom/miniagent/internal/miniagent/event"
	"github.com/justphantom/miniagent/internal/provider/openai"
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
		return 0, fmt.Errorf("run.http_timeout %q 负值不合法", *cfg.Run.HTTPTimeout)
	}
	return d, nil
}

// listAllModels 按 provider 解析 key（provider.Key > $MINIAGENT_API_KEY）并复用统一
// transport/timeout，聚合模型列表。
func listAllModels(ctx context.Context, providers []miniagent.ProviderConfig, httpTimeout time.Duration, logger *slog.Logger) ([]miniagent.ModelRef, error) {
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
	return openai.ListAllModels(ctx, providers, keyFor, httpClient, logger)
}

// runListModels 实现 -list-models 早退路径：逐行输出 NDJSON model 事件（provider/model
// 分离字段）。providerFilter 非空时仅列出该 provider。部分失败：成功条目照常输出，退出码 1。
func runListModels(ctx context.Context, cfg *miniagent.Config, providerFilter string, logger *slog.Logger) {
	listHTTPTimeout, err := httpTimeoutFromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: config: %v\n", err)
		os.Exit(1)
	}
	providers := cfg.Providers
	if providerFilter != "" {
		p, err := miniagent.FindProvider(cfg, providerFilter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
			os.Exit(1)
		}
		providers = []miniagent.ProviderConfig{p}
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
		fmt.Fprintf(os.Stderr, "miniagent: list models: %v\n", err)
		os.Exit(1)
	}
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
func buildLLM(apiKey string, p miniagent.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) (*openai.ChatClient, *openai.StreamClient) {
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
	return chat, stream
}

// buildChatClient 为指定 provider 构造非流式 ChatClient（用于 compaction 等仅需 Do 的场景）。
func buildChatClient(apiKey string, p miniagent.ProviderConfig, logger *slog.Logger, httpTimeout time.Duration) *openai.ChatClient {
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

// newHTTPTransport 返回复用的 *http.Transport，配置代理、dial、TLS、响应头超时。
func newHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		// 曾为 30s：慢端点（如 agnes）常在此砍断请求，致 compaction 摘要等长输入场景失败。
		// 放宽到 300s；副作用是任意 provider 的慢请求都会挂更久（与 http.Client.Timeout 共同生效）。
		ResponseHeaderTimeout: 300 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// newHTTPClient 返回带指定总 timeout 和 transport 的 *http.Client。
func newHTTPClient(timeout time.Duration, transport *http.Transport) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}
