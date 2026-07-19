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

func main() {
	var (
		model      = flag.String("model", "", "LLM model id (required for conversation)")
		baseURL    = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
		system     = flag.String("system", "你是一个简洁的助手，回答通常不超过 500 字。", "system prompt")
		maxTokens  = flag.Int("max-tokens", 4096, "max output tokens")
		workdir    = flag.String("workdir", "", "working directory (tool bounds + shell cwd)")
		stateDir   = flag.String("state-dir", "", "state directory for session/memory (empty = stateless)")
		chatID     = flag.String("chat-id", "", "chat id for per-chat session isolation (empty = no history)")
		permission = flag.String("permission", "default", "permission mode: plan (read-only), default (bounded), free (unrestricted)")
		verbose    = flag.Bool("verbose", false, "emit tool_use and tool_result events (default: tool_use only)")
		blockedPat = flag.String("blocked-patterns", "", "JSON array of blocked shell patterns (overrides built-in defaults)")
		showVer    = flag.Bool("version", false, "show version")

		listModels   = flag.Bool("list-models", false, "list available models from the endpoint, then exit")
		listSessions = flag.Bool("list-sessions", false, "list sessions for --chat-id, then exit")
		showCurrent  = flag.Bool("show-current", false, "show current session/model/dir/permission for --chat-id, then exit")
		useSession   = flag.String("use-session", "", "switch to session <id> for --chat-id, then exit")
		delSession   = flag.String("del-session", "", "delete session <id> for --chat-id, then exit")

		// Mutation subcommands. Each dispatches before the main conversation
		// flow. The -set-* flags read their value from the existing -model/
		// -workdir/-permission flags; to CLEAR a pin the bridge uses the
		// explicit -clear-* flag (necessary because -permission has a
		// non-empty default of "default", so empty-value-as-clear would be
		// ambiguous for that one flag and inconsistent for the others).
		newSession       = flag.Bool("new-session", false, "create a new session for --chat-id, then exit")
		setModel         = flag.Bool("set-model", false, "set the model pin for --chat-id (reads value from -model), then exit")
		clearModel       = flag.Bool("clear-model", false, "clear the model pin for --chat-id, then exit")
		setDir           = flag.Bool("set-dir", false, "set the directory pin for --chat-id (reads value from -workdir), then exit")
		clearDir         = flag.Bool("clear-dir", false, "clear the directory pin for --chat-id, then exit")
		setPerm          = flag.Bool("set-permission", false, "set the permission pin for --chat-id (reads value from -permission), then exit")
		clearPerm        = flag.Bool("clear-permission", false, "clear the permission pin for --chat-id, then exit")
		memoryList       = flag.Bool("memory-list", false, "list long-term facts for --chat-id, then exit")
		memoryDelete     = flag.String("memory-delete", "", "delete fact <key> for --chat-id, then exit")
		memorySearch     = flag.String("memory-search", "", "search facts by substring <query> for --chat-id, then exit")
		scope            = flag.String("scope", "chat", "scope for memory-* subcommands: chat|project|global")
		prefix           = flag.String("prefix", "", "key prefix filter for --memory-list")
		limit            = flag.Int("limit", 20, "max results for --memory-search")
	)
	flag.Parse()

	apiKey := os.Getenv("MINIAGENT_API_KEY")

	if *showVer {
		fmt.Printf("miniagent %s\n", version) //nolint:forbidigo // CLI 输出
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if *listModels {
		runListModels(apiKey, *baseURL)
		return
	}
	if *listSessions {
		runListSessions(*stateDir, *chatID)
		return
	}
	if *showCurrent {
		runShowCurrent(*stateDir, *chatID)
		return
	}
	if *useSession != "" {
		runUseSession(*stateDir, *chatID, *useSession)
		return
	}
	if *delSession != "" {
		runDelSession(*stateDir, *chatID, *delSession)
		return
	}
	if *newSession {
		runNewSession(*stateDir, *chatID)
		return
	}
	if *setModel {
		runSetModel(*stateDir, *chatID, *model)
		return
	}
	if *clearModel {
		runSetModel(*stateDir, *chatID, "")
		return
	}
	if *setDir {
		runSetDir(*stateDir, *chatID, *workdir)
		return
	}
	if *clearDir {
		runSetDir(*stateDir, *chatID, "")
		return
	}
	if *setPerm {
		runSetPermission(*stateDir, *chatID, *permission)
		return
	}
	if *clearPerm {
		runSetPermission(*stateDir, *chatID, "")
		return
	}
	if *memoryList {
		runMemoryList(*stateDir, *chatID, *scope, *prefix)
		return
	}
	if *memoryDelete != "" {
		runMemoryDelete(*stateDir, *chatID, *scope, *memoryDelete)
		return
	}
	if *memorySearch != "" {
		runMemorySearch(*stateDir, *chatID, *scope, *memorySearch, *limit)
		return
	}

	if *model == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --model is required (or use a metadata flag like --list-models)")
		os.Exit(1)
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: $MINIAGENT_API_KEY is required (or use a metadata flag like --list-models)")
		os.Exit(1)
	}

	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: read stdin: %v\n", err)
		os.Exit(1)
	}
	if len(prompt) == 0 {
		fmt.Fprintln(os.Stderr, "miniagent: stdin is empty (send prompt via pipe or redirect)")
		os.Exit(1)
	}

	llm := &miniagent.HTTPClient{
		APIKey:  apiKey,
		BaseURL: *baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Logger:  logger,
	}

	var blockedPats []string
	if *blockedPat != "" {
		if err := json.Unmarshal([]byte(*blockedPat), &blockedPats); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: --blocked-patterns parse error: %v\n", err)
			os.Exit(1)
		}
	}

	tools := buildTools(toolConfig{
		permission:      *permission,
		workdir:         *workdir,
		blockedPatterns: blockedPats,
	})

	st := initStores(*stateDir, *chatID, *model, *workdir, *permission, logger)
	if st.facts != nil {
		tools = append(tools, miniagent.MemoryTools(st.facts, *chatID)...)
	}
	hist := st.history.Load(*chatID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	emit := miniagent.StreamEmitFunc(os.Stdout, *verbose)

	memoryContext := ""
	if st.facts != nil {
		chatFacts, err := st.facts.List(miniagent.ScopeChat, *chatID, "")
		if err != nil {
			logger.Warn("memory: list failed", "error", err)
		} else if len(chatFacts) > 0 {
			memoryContext = formatFactsForCLI(chatFacts)
		}
	}

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:         *model,
		System:        *system,
		MemoryContext: memoryContext,
		MaxTokens:     *maxTokens,
		Tools:         tools,
	}, "cli", string(prompt), hist, emit, logger)

	if err != nil {
		if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
			logger.Warn("emit error failed", "error", eerr)
		}
		os.Exit(1)
	}

	if err := st.history.Append(*chatID, result.NewMessages); err != nil {
		logger.Warn("history: append failed", "error", err)
	}
	if result.Incomplete {
		logger.Warn("loop: hit max iterations; usage/history emitted but no final text", "steps", result.Steps)
	}
	if err := miniagent.EmitResult(os.Stdout, result, *model); err != nil {
		logger.Warn("emit result failed", "error", err)
		os.Exit(1)
	}
}
