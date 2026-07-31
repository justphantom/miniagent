// Command miniagent runs a single agent turn from stdin and emits NDJSON
// events to stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
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
	model       *string
	baseURL     *string
	system      *string
	maxTokens   *int
	maxDuration *time.Duration
	workdir     *string
	session     *string
	showVer     *bool
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.model = flag.String("model", "", "LLM model id (required)")
	f.baseURL = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
	f.system = flag.String("system", "你是一个简洁的助手，回答通常不超过 500 字。", "system prompt")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens per LLM call")
	f.maxDuration = flag.Duration("max-duration", 0, "overall wall-clock limit (0 = unlimited); covers all LLM calls + tool runs")
	f.workdir = flag.String("workdir", "", "working directory (tool path prefix + shell cwd)")
	f.session = flag.String("session", "", "session file for continuing conversation (JSON history, created if missing)")
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

	apiKey := os.Getenv("MINIAGENT_API_KEY")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	validateConversationFlags(f, apiKey)
	warnInsecureBaseURL(*f.baseURL)
	prompt := mustReadPrompt()
	history := mustLoadSession(*f.session)
	llm := buildLLM(apiKey, *f.baseURL, logger)
	tools := buildTools(*f.workdir)
	onToolUse := miniagent.ToolUseWriter(os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *f.maxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *f.maxDuration)
		defer cancel()
	}

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:     *f.model,
		System:    *f.system,
		MaxTokens: *f.maxTokens,
		Tools:     tools,
		History:   history,
	}, string(prompt), onToolUse, logger)
	if err != nil {
		// Run 出错时不写回 session：不把失败轮的半成品历史固化（工具的
		// 副作用已发生但无记录，是已接受的取舍）。
		if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
			logger.Warn("emit error failed", "error", eerr)
		}
		os.Exit(1)
	}
	if err := miniagent.EmitResult(os.Stdout, result, *f.model); err != nil {
		logger.Warn("emit result failed", "error", err)
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

func validateConversationFlags(f *cliFlags, apiKey string) {
	if *f.model == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --model is required")
		os.Exit(1)
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: $MINIAGENT_API_KEY is required")
		os.Exit(1)
	}
}

func mustReadPrompt() []byte {
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: read stdin: %v\n", err)
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
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return
	}
	fmt.Fprintf(os.Stderr, "miniagent: warning: base-url %q uses plain http, API key sent unencrypted\n", baseURL)
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

// buildTools 无条件注册 4 个工具。workdir 为空时工具内部按各自规则处理
// （read/write/edit 走 resolveToolPath，shell 把 cmd.Dir 留空继承 cwd）。
func buildTools(workdir string) []miniagent.Tool {
	return []miniagent.Tool{
		miniagent.ReadFileTool(workdir),
		miniagent.WriteFileTool(workdir),
		miniagent.EditFileTool(workdir),
		miniagent.ShellTool(workdir),
	}
}
