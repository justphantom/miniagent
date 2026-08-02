package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func mustParseLogLevel(s string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid -log-level %q (want debug|info|warn|error)\n", s)
		os.Exit(1)
	}
	return level
}

// minimalConfigTemplate 是无 config 时写盘的最小模板：${CHAT_URL}/${MODEL} 来自 env
// （忠实「始终存在 miniagent.json」，S1 删裸模式后的默认入口）。
const minimalConfigTemplate = `{
  "providers": [{"name": "default", "chat_url": "${CHAT_URL}"}],
  "defaults": {"model": "${MODEL}"}}
`

// requireConfig：始终要求 config 存在（S1 删裸模式）。显式 -config 不存在=硬错误；
// 默认 ./miniagent.json 不存在=写最小模板再加载（${CHAT_URL}/${MODEL} 须在 env，否则 expandVars 报错）。
//
// 默认 config 的 Stat 错误精确区分：仅 fs.ErrNotExist 触发模板生成；permission denied/
// ELOOP/ENOTDIR 等按硬错误上抛——否则用户在权限/路径异常下被静默切到无 tool 边界、
// 无 system prompt 注入的配置（审查 P2-6）。
func requireConfig(configPath string) (*miniagent.Config, error) {
	if configPath != "" {
		return miniagent.LoadConfig(configPath)
	}
	if _, err := os.Stat("./miniagent.json"); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("stat 默认 config %q: %w", "./miniagent.json", err)
		}
		if werr := os.WriteFile("./miniagent.json", []byte(minimalConfigTemplate), 0o600); werr != nil {
			return nil, fmt.Errorf("生成默认 config ./miniagent.json 失败：%w（可手写 miniagent.json 或用 -config）", werr)
		}
	}
	return miniagent.LoadConfig("./miniagent.json")
}

// collectOverrides 用 flag.Visit 收集「显式传入」的 flag（未传入置 nil），供 Resolve 裁决。
// P2 后仅保留 CLI 核心参数；策略参数（summary/duration/window 等）只在 config，不经 CLI。
func collectOverrides(f *cliFlags) miniagent.CLIOverrides {
	set := map[string]bool{}
	flag.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	o := miniagent.CLIOverrides{}
	if set["model"] {
		o.Model = f.model
	}
	if set["thinking"] {
		o.Thinking = f.thinking
	}
	if set["mode"] {
		o.Mode = f.mode
	}
	if set["system"] {
		o.System = f.system
	}
	if set["workdir"] {
		o.Workdir = f.workdir
	}
	if set["session"] {
		o.Session = f.session
	}
	if set["max-tokens"] {
		o.MaxTokens = f.maxTokens
	}
	if set["max-iterations"] {
		o.MaxIterations = f.maxIterations
	}
	if set["stream"] {
		o.Stream = f.stream
	}
	if set["result-only"] {
		o.ResultOnly = f.resultOnly
	}
	return o
}

// resolveFinalKey：cli(-key-file) > config(provider.Key) > env。机密不入 config 文件——
// provider.Key 通常是 ${VAR} 展开，仍来自环境。
func resolveFinalKey(providerKey, keyFile string) string {
	if keyFile != "" {
		key, err := resolveAPIKey(keyFile, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
			os.Exit(1)
		}
		warnKeyFilePerm(keyFile)
		return key
	}
	if providerKey != "" {
		return providerKey
	}
	return os.Getenv("MINIAGENT_API_KEY")
}

func resolveAPIKey(keyFile, envKey string) (string, error) {
	if keyFile == "" {
		return envKey, nil
	}
	data, err := readKeyFileNoFollow(keyFile)
	if err != nil {
		return "", fmt.Errorf("read key-file %q: %w", keyFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func warnKeyFilePerm(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "miniagent: warning: key-file %s readable by group/other (mode=%o); recommend 0600\n", path, info.Mode().Perm())
	}
}

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
	return d, nil
}

// listAllModels 按 provider 解析 key 并复用统一 transport/timeout，聚合模型列表。
func listAllModels(ctx context.Context, providers []miniagent.ProviderConfig, keyFile string, httpTimeout time.Duration, logger *slog.Logger) ([]string, error) {
	keyFileKey := ""
	if keyFile != "" {
		var err error
		keyFileKey, err = resolveAPIKey(keyFile, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
			os.Exit(1)
		}
		warnKeyFilePerm(keyFile)
	}
	keyFor := func(p miniagent.ProviderConfig) string {
		if keyFileKey != "" {
			return keyFileKey
		}
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
	chat, err := miniagent.NewChatClient(apiKey, p.ChatURL, p.ModelsURL, chatClient, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	stream, err := miniagent.NewStreamClient(apiKey, p.ChatURL, streamClient, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid endpoint url: %v\n", err)
		os.Exit(1)
	}
	return chat, stream
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

func validateConversation(resolved *miniagent.Resolved, f *cliFlags) {
	if *f.stream && *f.resultOnly {
		fmt.Fprintln(os.Stderr, "miniagent: -stream 与 -result-only 互斥")
		os.Exit(1)
	}
	if resolved.Mode == miniagent.ModeDefault && effectiveWorkdir(resolved, f) == "" {
		fmt.Fprintln(os.Stderr, "miniagent: default 模式需 -workdir（或 config run.workdir，或用 -mode auto）")
		os.Exit(1)
	}
}

func effectiveWorkdir(resolved *miniagent.Resolved, f *cliFlags) string {
	if resolved.Run.Workdir != nil && *resolved.Run.Workdir != "" {
		return *resolved.Run.Workdir
	}
	return *f.workdir
}

func maxDurationOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.MaxDuration != nil {
		return *resolved.Run.MaxDuration
	}
	return 0
}

func shellTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.ShellTimeout != nil {
		return *resolved.Run.ShellTimeout
	}
	return 0
}

func fileOpTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.FileOpTimeout != nil {
		return *resolved.Run.FileOpTimeout
	}
	return 0
}

func writeTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.WriteTimeout != nil {
		return *resolved.Run.WriteTimeout
	}
	return 0
}

func httpTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.HTTPTimeout != nil {
		return *resolved.Run.HTTPTimeout
	}
	return 0
}

func maxReadFileBytesOf(resolved *miniagent.Resolved) int {
	if resolved.Run.MaxReadFileBytes != nil {
		return *resolved.Run.MaxReadFileBytes
	}
	return 0
}

func maxShellOutputCharsOf(resolved *miniagent.Resolved) int {
	if resolved.Run.MaxShellOutputChars != nil {
		return *resolved.Run.MaxShellOutputChars
	}
	return 0
}

func maxSessionBytesOf(resolved *miniagent.Resolved) int {
	if resolved.Run.MaxSessionBytes != nil {
		return *resolved.Run.MaxSessionBytes
	}
	return 0
}

func buildHooks(resultOnly bool) miniagent.LoopHooks {
	if resultOnly {
		// subagent fork：stdout 纯文本即结果，不发 NDJSON 事件。
		return miniagent.LoopHooks{}
	}
	emit := miniagent.ToolUseWriter(os.Stdout)
	return miniagent.LoopHooks{
		OnToolUse: func(name, input string) error { return emit(name, input) },
		OnToolResult: func(name, callID string, r miniagent.ToolResult) error {
			return miniagent.EmitToolResult(os.Stdout, name, callID, r)
		},
		OnDelta: func(step int, kind miniagent.DeltaKind, text string) error {
			return miniagent.EmitDelta(os.Stdout, step, kind, text)
		},
	}
}

func emitRunError(err error, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Printf("error: %s\n", err.Error())
		return
	}
	if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
		logger.Warn("emit error failed", "error", eerr)
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
	}
}

func emitRunResult(result miniagent.Result, model string, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Println(result.Text)
		return
	}
	if err := miniagent.EmitResult(os.Stdout, result, model); err != nil {
		logger.Warn("emit result failed", "error", err)
		fmt.Fprintf(os.Stderr, "miniagent: emit result failed: %v (text: %.200q)\n", err, result.Text)
		os.Exit(1)
	}
}

// providerForListModels 解析 -list-models 所需 provider（不要求 -model，因 list 本就为发现模型）：
// 按 -model/defaults.model/单一 provider。多 provider 时由 main.go 走聚合路径，此函数仅用于单 provider 分支。
func providerForListModels(cfg *miniagent.Config, f *cliFlags) (miniagent.ProviderConfig, error) {
	spec := ""
	if f.model != nil && *f.model != "" {
		spec = *f.model
	} else {
		spec = cfg.Defaults.Model
	}
	if spec != "" {
		p, _, err := miniagent.ParseModelSpec(spec, cfg)
		return p, err
	}
	if len(cfg.Providers) == 1 {
		return cfg.Providers[0], nil
	}
	return miniagent.ProviderConfig{}, errors.New("list-models 需 -model 或 defaults.model 或单一 provider")
}

// absConfigPath 返回实际加载的 config 绝对路径（显式 -config 或默认 ./miniagent.json），
// 供 subagent fork 引导注入。cfg 始终非 nil（S1 删裸模式）。
func absConfigPath(configPath string) string {
	p := configPath
	if p == "" {
		p = "./miniagent.json"
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
