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
	"strings"
	"syscall"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

var version = "dev"

func main() {
	var (
		model      = flag.String("model", "", "LLM model id (required for conversation)")
		apiKey     = flag.String("api-key", os.Getenv("MINIAGENT_API_KEY"), "LLM API key (or $MINIAGENT_API_KEY)")
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
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("miniagent %s\n", version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if *listModels {
		runListModels(*apiKey, *baseURL)
		return
	}
	if *listSessions {
		runListSessions(*stateDir, *chatID)
		return
	}
	if *showCurrent {
		runShowCurrent(*stateDir, *chatID, *model)
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

	if *model == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --model is required (or use a metadata flag like --list-models)")
		os.Exit(1)
	}
	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --api-key is required (or set $MINIAGENT_API_KEY)")
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
		APIKey:  *apiKey,
		BaseURL: *baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Logger:  logger,
	}

	unrestricted := *permission == "free"
	var blockedPats []string
	if *blockedPat != "" {
		if err := json.Unmarshal([]byte(*blockedPat), &blockedPats); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: --blocked-patterns parse error: %v\n", err)
			os.Exit(1)
		}
	}

	var tools []miniagent.Tool
	switch *permission {
	case "plan":
		if *workdir != "" {
			tools = append(tools, miniagent.ReadFileTool(*workdir, unrestricted))
		} else {
			fmt.Fprintln(os.Stderr, "miniagent: --workdir is empty, read_file not registered (plan mode needs a workspace)")
		}
		tools = append(tools, miniagent.WebFetchTool(nil))
	default:
		if *workdir != "" || unrestricted {
			tools = append(tools,
				miniagent.ReadFileTool(*workdir, unrestricted),
				miniagent.WriteFileTool(*workdir, unrestricted),
				miniagent.EditFileTool(*workdir, unrestricted),
				miniagent.ShellTool(*workdir, unrestricted, blockedPats),
			)
		} else {
			fmt.Fprintln(os.Stderr, "miniagent: --workdir is empty AND permission is not free; read_file/write_file/shell/edit_file not registered")
		}
		tools = append(tools, miniagent.WebFetchTool(nil))
	}

	var history *miniagent.History
	var facts *miniagent.FactStore
	if *stateDir != "" && *chatID != "" {
		history = miniagent.NewHistory(*stateDir, logger)
		facts = miniagent.NewFactStore(*stateDir, logger)
		tools = append(tools, miniagent.MemoryTools(facts, *chatID)...)
	}
	hist := history.Load(*chatID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	emit := miniagent.StreamEmitFunc(os.Stdout, *verbose)

	memoryContext := ""
	if facts != nil {
		if chatFacts, _ := facts.List(miniagent.ScopeChat, *chatID, ""); len(chatFacts) > 0 {
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
		miniagent.EmitError(os.Stdout, err.Error())
		os.Exit(1)
	}

	history.Append(*chatID, result.History)
	miniagent.EmitResult(os.Stdout, result, *model)
}

func formatFactsForCLI(facts []miniagent.Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n以下是与当前对话相关的已知事实（由用户或之前的对话沉淀）：\n")
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return sb.String()
}

func runListModels(apiKey, baseURL string) {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --api-key is required for --list-models")
		os.Exit(1)
	}
	c := &miniagent.HTTPClient{APIKey: apiKey, BaseURL: baseURL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := c.ListModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: list models: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(models)
	fmt.Println(string(out))
}

func mustHistory(stateDir, chatID, action string) *miniagent.History {
	if stateDir == "" {
		fmt.Fprintf(os.Stderr, "miniagent: --state-dir is required for --%s\n", action)
		os.Exit(1)
	}
	if chatID == "" {
		fmt.Fprintf(os.Stderr, "miniagent: --chat-id is required for --%s\n", action)
		os.Exit(1)
	}
	return miniagent.NewHistory(stateDir, nil)
}

func runListSessions(stateDir, chatID string) {
	h := mustHistory(stateDir, chatID, "list-sessions")
	sessions, err := h.ListSessions(chatID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: list sessions: %v\n", err)
		os.Exit(1)
	}
	type sessionOut struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
		Bytes   int64  `json:"bytes"`
		ModTime string `json:"mod_time"`
	}
	out := make([]sessionOut, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionOut{ID: s.ID, Current: s.Current, Bytes: s.Bytes, ModTime: s.ModTime.Format("2006-01-02 15:04:05")})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func runShowCurrent(stateDir, chatID, defaultModel string) {
	h := mustHistory(stateDir, chatID, "show-current")
	info := struct {
		ChatID     string `json:"chat_id"`
		SessionID  string `json:"session_id"`
		Model      string `json:"model"`
		Directory  string `json:"directory"`
		Permission string `json:"permission"`
	}{
		ChatID:     chatID,
		SessionID:  h.Current(chatID),
		Model:      h.Model(chatID),
		Directory:  h.Directory(chatID),
		Permission: h.Permission(chatID),
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(b))
}

func runUseSession(stateDir, chatID, sid string) {
	h := mustHistory(stateDir, chatID, "use-session")
	if err := h.UseSession(chatID, sid); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: use session: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("switched to session %s\n", sid)
}

func runDelSession(stateDir, chatID, sid string) {
	h := mustHistory(stateDir, chatID, "del-session")
	if err := h.DeleteSession(chatID, sid); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: delete session: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("deleted session %s\n", sid)
}
