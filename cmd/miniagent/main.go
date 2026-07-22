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
	showCurrent  *bool
	useSession   *string
	delSession   *string

	newSession   *bool
	setModel     *bool
	clearModel   *bool
	setDir       *bool
	clearDir     *bool
	setPerm      *bool
	clearPerm    *bool

	stream *bool

	maxParallelTools *int
	maxTokensBudget  *int
	maxHistoryTokens *int
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
	f.showCurrent = flag.Bool("show-current", false, "show current session/model/dir/permission for --chat-id, then exit")
	f.useSession = flag.String("use-session", "", "switch to session <id> for --chat-id, then exit")
	f.delSession = flag.String("del-session", "", "delete session <id> for --chat-id, then exit")

	// Mutation subcommands. Each dispatches before the main conversation
	// flow. The -set-* flags read their value from the existing -model/
	// -workdir/-permission flags; to CLEAR a pin the bridge uses the
	// explicit -clear-* flag (necessary because -permission has a
	// non-empty default of "default", so empty-value-as-clear would be
	// ambiguous for that one flag and inconsistent for the others).
	f.newSession = flag.Bool("new-session", false, "create a new session for --chat-id, then exit")
	f.setModel = flag.Bool("set-model", false, "set the model pin for --chat-id (reads value from -model), then exit")
	f.clearModel = flag.Bool("clear-model", false, "clear the model pin for --chat-id, then exit")
	f.setDir = flag.Bool("set-dir", false, "set the directory pin for --chat-id (reads value from -workdir), then exit")
	f.clearDir = flag.Bool("clear-dir", false, "clear the directory pin for --chat-id, then exit")
	f.setPerm = flag.Bool("set-permission", false, "set the permission pin for --chat-id (reads value from -permission), then exit")
	f.clearPerm = flag.Bool("clear-permission", false, "clear the permission pin for --chat-id, then exit")

	// 默认开启：流式下首字节快、可中途感知。端点不支持 SSE 时用 -stream=false 回退非流式。
	f.stream = flag.Bool("stream", true, "stream SSE text deltas (default true)")

	// 护栏与上下文预算：默认值见 internal/miniagent 对应常量；0 表示沿用默认或不限。
	f.maxParallelTools = flag.Int("max-parallel-tools", 8, "max concurrent tool calls within one step (0 = unlimited)")
	f.maxTokensBudget = flag.Int("max-tokens-budget", 0, "per-turn cumulative input+output token cap; stops with incomplete result (0 = unlimited)")
	f.maxHistoryTokens = flag.Int("max-history-tokens", 0, "history trimming token budget (0 = default 6000)")

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
	if dispatchMetadataSubcommand(f, apiKey) {
		return true
	}
	return dispatchMutationSubcommand(f)
}

func dispatchMetadataSubcommand(f *cliFlags, apiKey string) bool {
	if *f.listModels {
		runListModels(apiKey, *f.baseURL)
		return true
	}
	if *f.listSessions {
		runListSessions(*f.stateDir, *f.chatID)
		return true
	}
	if *f.showCurrent {
		runShowCurrent(*f.stateDir, *f.chatID)
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
	return false
}

func dispatchMutationSubcommand(f *cliFlags) bool {
	if *f.newSession {
		runNewSession(*f.stateDir, *f.chatID)
		return true
	}
	if *f.setModel {
		runSetModel(*f.stateDir, *f.chatID, *f.model)
		return true
	}
	if *f.clearModel {
		runSetModel(*f.stateDir, *f.chatID, "")
		return true
	}
	if *f.setDir {
		runSetDir(*f.stateDir, *f.chatID, *f.workdir)
		return true
	}
	if *f.clearDir {
		runSetDir(*f.stateDir, *f.chatID, "")
		return true
	}
	if *f.setPerm {
		runSetPermission(*f.stateDir, *f.chatID, *f.permission)
		return true
	}
	if *f.clearPerm {
		runSetPermission(*f.stateDir, *f.chatID, "")
		return true
	}
	return false
}

func runConversation(f *cliFlags, apiKey string, logger *slog.Logger) {
	validateConversationFlags(f, apiKey)
	prompt := mustReadPrompt()
	llm := buildLLM(apiKey, *f.baseURL, logger)
	tools := buildToolSet(f)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result := mustRunAgent(ctx, llm, f, tools, prompt, logger)
	emitConversationResult(result, f, logger)
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

func mustRunAgent(ctx context.Context, llm *miniagent.HTTPClient, f *cliFlags, tools []miniagent.Tool, prompt []byte, logger *slog.Logger) miniagent.Result {
	st := initStores(*f.stateDir, *f.chatID, *f.model, *f.workdir, *f.permission, logger)
	hist := st.history.Load(*f.chatID)
	emit := miniagent.StreamEmitFunc(os.Stdout, *f.verbose)

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:            *f.model,
		System:           *f.system,
		MaxTokens:        *f.maxTokens,
		Tools:            tools,
		Stream:           *f.stream,
		MaxParallelTools: *f.maxParallelTools,
		MaxTokensBudget:  *f.maxTokensBudget,
		MaxHistoryTokens: *f.maxHistoryTokens,
	}, "cli", string(prompt), hist, emit, logger)

	if err != nil {
		if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
			logger.Warn("emit error failed", "error", eerr)
		}
		os.Exit(1)
	}
	return result
}

func emitConversationResult(result miniagent.Result, f *cliFlags, logger *slog.Logger) {
	st := initStores(*f.stateDir, *f.chatID, *f.model, *f.workdir, *f.permission, logger)
	if err := st.history.Append(*f.chatID, result.NewMessages); err != nil {
		logger.Warn("history: append failed", "error", err)
	}
	if result.Incomplete {
		logger.Warn("loop: hit max iterations; usage/history emitted but no final text", "steps", result.Steps)
	}
	if err := miniagent.EmitResult(os.Stdout, result, *f.model); err != nil {
		logger.Warn("emit result failed", "error", err)
		os.Exit(1)
	}
}
