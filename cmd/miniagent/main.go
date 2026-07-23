// Command miniagent runs a single agent turn from stdin and emits NDJSON
// events to stdout.
package main

import (
	"context"
	"encoding/json"
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
	model      *string
	baseURL    *string
	system     *string
	maxTokens  *int
	workdir    *string
	stateDir   *string
	chatID     *string
	permission *string
	verbose    *bool
	blockedPat *string
	showVer    *bool

	listModels   *bool
	listSessions *bool
	useSession   *string
	delSession   *string

	newSession *bool
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.model = flag.String("model", "", "LLM model id (required for conversation)")
	f.baseURL = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
	f.system = flag.String("system", "你是一个简洁的助手，回答通常不超过 500 字。", "system prompt")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens")
	f.workdir = flag.String("workdir", "", "working directory (tool bounds + shell cwd)")
	f.stateDir = flag.String("state-dir", "", "state directory for sessions (empty = stateless)")
	f.chatID = flag.String("chat-id", "", "chat id for per-chat session isolation (empty = no history)")
	f.permission = flag.String("permission", "default", "permission mode: default (bounded) or free (unrestricted)")
	f.verbose = flag.Bool("verbose", false, "emit tool_use and tool_result events (default: tool_use only)")
	f.blockedPat = flag.String("blocked-patterns", "", "JSON array of blocked shell patterns (overrides built-in defaults)")
	f.showVer = flag.Bool("version", false, "show version")

	f.listModels = flag.Bool("list-models", false, "list available models from the endpoint, then exit")
	f.listSessions = flag.Bool("list-sessions", false, "list sessions for --chat-id, then exit")
	f.useSession = flag.String("use-session", "", "switch to session <id> for --chat-id, then exit")
	f.delSession = flag.String("del-session", "", "delete session <id> for --chat-id, then exit")
	f.newSession = flag.Bool("new-session", false, "create a new session for --chat-id, then exit")

	flag.Parse()
	return f
}

func main() {
	f := parseFlags()
	apiKey := os.Getenv("MINIAGENT_API_KEY")

	if *f.showVer {
		fmt.Printf("miniagent %s\n", version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if dispatchSubcommand(f, apiKey) {
		return
	}

	runConversation(f, apiKey, logger)
}

func dispatchSubcommand(f *cliFlags, apiKey string) bool {
	if *f.listModels {
		runListModels(apiKey, *f.baseURL)
		return true
	}
	if *f.listSessions {
		runListSessions(*f.stateDir, *f.chatID)
		return true
	}
	if *f.useSession != "" {
		runUseSession(*f.stateDir, *f.chatID, *f.useSession)
		return true
	}
	if *f.delSession != "" {
		runDelSession(*f.stateDir, *f.chatID, *f.delSession)
		return true
	}
	if *f.newSession {
		runNewSession(*f.stateDir, *f.chatID)
		return true
	}
	return false
}

func runConversation(f *cliFlags, apiKey string, logger *slog.Logger) {
	validateConversationFlags(f, apiKey)
	prompt := mustReadPrompt()
	llm := buildLLM(apiKey, *f.baseURL, logger)
	tools := buildToolSet(f)
	hist := initHistory(*f.stateDir, *f.chatID, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result := mustRunAgent(ctx, llm, f, hist, tools, prompt, logger)
	emitConversationResult(result, f, hist, logger)
}

func validateConversationFlags(f *cliFlags, apiKey string) {
	if *f.model == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --model is required (or use a metadata flag like --list-models)")
		os.Exit(1)
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: $MINIAGENT_API_KEY is required (or use a metadata flag like --list-models)")
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

func buildToolSet(f *cliFlags) []miniagent.Tool {
	var blockedPats []string
	if *f.blockedPat != "" {
		if err := json.Unmarshal([]byte(*f.blockedPat), &blockedPats); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: --blocked-patterns parse error: %v\n", err)
			os.Exit(1)
		}
	}

	tools := buildTools(toolConfig{
		permission:      *f.permission,
		workdir:         *f.workdir,
		blockedPatterns: blockedPats,
	})
	return tools
}

func mustRunAgent(ctx context.Context, llm *miniagent.HTTPClient, f *cliFlags, hist *miniagent.History, tools []miniagent.Tool, prompt []byte, logger *slog.Logger) miniagent.Result {
	loaded := hist.Load(*f.chatID)
	emit := miniagent.StreamEmitFunc(os.Stdout, *f.verbose)

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:     *f.model,
		System:    *f.system,
		MaxTokens: *f.maxTokens,
		Tools:     tools,
	}, "cli", string(prompt), loaded, emit, logger)

	if err != nil {
		if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
			logger.Warn("emit error failed", "error", eerr)
		}
		os.Exit(1)
	}
	return result
}

func emitConversationResult(result miniagent.Result, f *cliFlags, hist *miniagent.History, logger *slog.Logger) {
	if err := hist.Append(*f.chatID, result.NewMessages); err != nil {
		logger.Warn("history: append failed", "error", err)
	}
	if err := miniagent.EmitResult(os.Stdout, result, *f.model); err != nil {
		logger.Warn("emit result failed", "error", err)
		os.Exit(1)
	}
}
