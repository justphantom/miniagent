// Command miniagent runs a single agent turn from stdin and emits NDJSON
// events to stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

var version = "dev"

type cliFlags struct {
	model          *string
	baseURL        *string
	keyFile        *string
	system         *string
	maxTokens      *int
	maxDuration    *time.Duration
	workdir        *string
	session        *string
	logLevel       *string
	showVer        *bool
	maxIterations  *int
	shellTimeout   *time.Duration
	maxTokensTotal *int
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.model = flag.String("model", "", "LLM model id (required)")
	f.baseURL = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
	f.keyFile = flag.String("key-file", "", "从文件读取 API key（首尾空白截断）；规避环境变量经 /proc/$PPID/environ 泄漏给 shell 子进程；优先于 $MINIAGENT_API_KEY")
	f.system = flag.String("system", defaultSystemPrompt, "system prompt")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens per LLM call")
	f.maxDuration = flag.Duration("max-duration", 0, "overall wall-clock limit (0 = unlimited); covers all LLM calls + tool runs")
	f.workdir = flag.String("workdir", "", "working directory (tool path prefix + shell cwd)")
	f.session = flag.String("session", "", "session file for continuing conversation (JSON history, created if missing)")
	f.logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
	f.maxIterations = flag.Int("max-iterations", 0, "单轮 LLM 调用上限（0=默认 20）")
	f.shellTimeout = flag.Duration("shell-timeout", 0, "单条 shell 命令超时（0=默认 60s）；仍受 -max-duration 总上限约束")
	f.maxTokensTotal = flag.Int("max-tokens-total", 0, "单轮累计 token（输入+输出）上限（0=不限）；超限以 error 事件 + 退出码 1 终止")
	f.showVer = flag.Bool("version", false, "show version")
	flag.Parse()
	return f
}

func main() {
	f := parseFlags()

	if *f.showVer {
		fmt.Printf("miniagent %s\n", version)
		os.Exit(0)
	}

	apiKey := mustLoadAPIKey(f)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: mustParseLogLevel(*f.logLevel)}))

	validateConversationFlags(f, apiKey)
	warnInsecureBaseURL(*f.baseURL)
	prompt := mustReadPrompt()
	history := mustLoadSession(*f.session)
	llm := buildLLM(apiKey, *f.baseURL, logger)
	tools := buildTools(*f.workdir, *f.shellTimeout)
	onToolUse := miniagent.ToolUseWriter(os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *f.maxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *f.maxDuration)
		defer cancel()
	}

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:          *f.model,
		System:         *f.system,
		MaxTokens:      *f.maxTokens,
		Tools:          tools,
		History:        history,
		MaxIterations:  *f.maxIterations,
		MaxTotalTokens: *f.maxTokensTotal,
	}, string(prompt), onToolUse, logger)
	if err != nil {
		// Run 出错时不写回 session：不把失败轮的半成品历史固化（工具的
		// 副作用已发生但无记录，是已接受的取舍）。
		if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
			logger.Warn("emit error failed", "error", eerr)
			// stdout 终态事件未送达（消费方提前关管道等），至少把原始错误落地到
			// stderr，避免消费方只剩退出码、丢失错误链。
			fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		}
		os.Exit(1)
	}
	if err := miniagent.EmitResult(os.Stdout, result, *f.model); err != nil {
		logger.Warn("emit result failed", "error", err)
		// result 终态事件未送达 stdout，兜底把文本摘要写 stderr，便于排障。
		fmt.Fprintf(os.Stderr, "miniagent: emit result failed: %v (text: %.200q)\n", err, result.Text)
		os.Exit(1)
	}
	// result 已送达消费方后再持久化：写回失败只影响下次接续，用退出码 1
	// 显式告知，不吞错也不丢本轮回答。
	if *f.session != "" {
		if err := miniagent.SaveSession(*f.session, result.Messages); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", err)
			os.Exit(1)
		}
	}
}

func mustParseLogLevel(s string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid -log-level %q (want debug|info|warn|error)\n", s)
		os.Exit(1)
	}
	return level
}

func validateConversationFlags(f *cliFlags, apiKey string) {
	if *f.model == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --model is required")
		os.Exit(1)
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: $MINIAGENT_API_KEY is required (or use -key-file)")
		os.Exit(1)
	}
}

// resolveAPIKey 解析 API key：-key-file 非空时从文件读（首尾空白截断），否则回退
// 到 env。-key-file 优先于 env。文件读失败返回 error（不由调用方静默吞）。
func resolveAPIKey(keyFile, envKey string) (string, error) {
	if keyFile == "" {
		return envKey, nil
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return "", fmt.Errorf("read key-file %q: %w", keyFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// mustLoadAPIKey 是 resolveAPIKey 的退出包装：读失败即 stderr + 退出码 1。
// 用 -key-file 时顺带校验文件权限（loose 则警告）。
func mustLoadAPIKey(f *cliFlags) string {
	key, err := resolveAPIKey(*f.keyFile, os.Getenv("MINIAGENT_API_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
	if *f.keyFile != "" {
		warnKeyFilePerm(*f.keyFile)
	}
	return key
}

// warnKeyFilePerm：key 文件若可被 group/other 读，stderr 警告（不强制——文件权限
// 与运行用户隔离由调用方保证）。
func warnKeyFilePerm(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "miniagent: warning: key-file %s readable by group/other (mode=%o); recommend 0600\n", path, info.Mode().Perm())
	}
}

// maxPromptBytes 是 stdin prompt 的大小上限：无上限读取既撑内存，写回
// session 后又会撞 LoadSession 的 maxSessionBytes 上限，导致会话永久无法
// 接续。取值小于 session 上限，给后续轮次的 transcript 增长留出空间。
const maxPromptBytes = 1 << 20

func mustReadPrompt() []byte {
	prompt, err := io.ReadAll(io.LimitReader(os.Stdin, maxPromptBytes+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: read stdin: %v\n", err)
		os.Exit(1)
	}
	if len(prompt) > maxPromptBytes {
		fmt.Fprintf(os.Stderr, "miniagent: stdin prompt 超过大小上限 %d 字节\n", maxPromptBytes)
		os.Exit(1)
	}
	if len(prompt) == 0 {
		fmt.Fprintln(os.Stderr, "miniagent: stdin is empty (send prompt via pipe or redirect)")
		os.Exit(1)
	}
	return prompt
}

// warnInsecureBaseURL：http（非 loopback）时 API key 明文上链，stderr 警告。
// 不强制拒绝：本地 vLLM/Ollama 是合法场景。
func warnInsecureBaseURL(baseURL string) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" {
		return
	}
	// IsLoopback 覆盖整个 127.0.0.0/8 与 ::1，字面量枚举会漏 127.0.0.2 等。
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	// Redacted 剥离 userinfo：base-url 形如 http://user:pass@host 时，%q 会把
	// 基本认证凭证一起落入 stderr（可能被日志聚合扩散）。
	fmt.Fprintf(os.Stderr, "miniagent: warning: base-url %s uses plain http, API key sent unencrypted\n", u.Redacted())
}

func mustLoadSession(path string) []miniagent.Message {
	if path == "" {
		return nil
	}
	history, err := miniagent.LoadSession(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: load session: %v\n", err)
		os.Exit(1)
	}
	return history
}

func buildLLM(apiKey, baseURL string, logger *slog.Logger) *miniagent.HTTPClient {
	return &miniagent.HTTPClient{
		APIKey:  apiKey,
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Logger:  logger,
	}
}

// buildTools 无条件注册 6 个工具。workdir 为空时工具内部按各自规则处理
// （read/write/edit/grep/glob 走 resolveToolPath，shell 把 cmd.Dir 留空继承 cwd）。
// shellTimeout<=0 时 ShellTool 用默认 60s。
func buildTools(workdir string, shellTimeout time.Duration) []miniagent.Tool {
	return []miniagent.Tool{
		miniagent.ReadFileTool(workdir),
		miniagent.WriteFileTool(workdir),
		miniagent.EditFileTool(workdir),
		miniagent.GrepTool(workdir),
		miniagent.GlobTool(workdir),
		miniagent.ShellTool(workdir, shellTimeout),
	}
}
