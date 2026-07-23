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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

var version = "dev"

type cliFlags struct {
	model     *string
	baseURL   *string
	system    *string
	maxTokens *int
	workdir   *string
	showVer   *bool
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.model = flag.String("model", "", "LLM model id (required)")
	f.baseURL = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
	f.system = flag.String("system", "你是一个简洁的助手，回答通常不超过 500 字。", "system prompt")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens")
	f.workdir = flag.String("workdir", "", "working directory (tool path prefix + shell cwd)")
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
	prompt := mustReadPrompt()
	llm := buildLLM(apiKey, *f.baseURL, logger)
	tools := buildTools(*f.workdir)
	emit := miniagent.StreamEmitFunc(os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:     *f.model,
		System:    *f.system,
		MaxTokens: *f.maxTokens,
		Tools:     tools,
	}, string(prompt), emit, logger)
	if err != nil {
		if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
			logger.Warn("emit error failed", "error", eerr)
		}
		os.Exit(1)
	}
	if err := miniagent.EmitResult(os.Stdout, result, *f.model); err != nil {
		logger.Warn("emit result failed", "error", err)
		os.Exit(1)
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

func buildLLM(apiKey, baseURL string, logger *slog.Logger) *miniagent.HTTPClient {
	return &miniagent.HTTPClient{
		APIKey:  apiKey,
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Logger:  logger,
	}
}
