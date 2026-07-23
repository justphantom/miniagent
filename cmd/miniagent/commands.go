package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func runListModels(apiKey, baseURL string) {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: $MINIAGENT_API_KEY is required for --list-models")
		os.Exit(1)
	}
	c := &miniagent.HTTPClient{APIKey: apiKey, BaseURL: baseURL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	models, err := c.ListModels(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: list models: %v\n", err)
		os.Exit(1)
	}
	out, err := json.Marshal(models)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: marshal models: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

// mustStoreArgs 校验子命令必需的 state-dir/chat-id，缺失即退出。
func mustStoreArgs(stateDir, chatID, action string) {
	if stateDir == "" {
		fmt.Fprintf(os.Stderr, "miniagent: --state-dir is required for --%s\n", action)
		os.Exit(1)
	}
	if chatID == "" {
		fmt.Fprintf(os.Stderr, "miniagent: --chat-id is required for --%s\n", action)
		os.Exit(1)
	}
}

func mustHistory(stateDir, chatID, action string) *miniagent.History {
	mustStoreArgs(stateDir, chatID, action)
	h, err := miniagent.NewHistory(stateDir, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init history: %v\n", err)
		os.Exit(1)
	}
	return h
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
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: marshal sessions: %v\n", err)
		os.Exit(1)
	}
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

func runNewSession(stateDir, chatID string) {
	h := mustHistory(stateDir, chatID, "new-session")
	sid, err := h.NewSession(chatID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: new session: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(mustMarshalJSON(map[string]string{"session_id": sid}))
}

func mustMarshalJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: marshal output: %v\n", err)
		os.Exit(1)
	}
	return string(b)
}

// toolConfig 是 buildTools 的入参，把 main 的 flag 解析结果集中传递，
// 避免函数签名挂一长串 *string。
type toolConfig struct {
	permission      string
	workdir         string
	blockedPatterns []string
}

// buildTools 按权限模式装配工具集。空 workdir 且非 free 时打印 stderr
// 告警并返回空工具集（与历史行为一致）。
func buildTools(cfg toolConfig) []miniagent.Tool {
	unrestricted := cfg.permission == "free"
	if cfg.workdir == "" && !unrestricted {
		fmt.Fprintln(os.Stderr, "miniagent: --workdir is empty AND permission is not free; read_file/write_file/shell/edit_file not registered")
		return nil
	}
	return []miniagent.Tool{
		miniagent.ReadFileTool(cfg.workdir, unrestricted),
		miniagent.WriteFileTool(cfg.workdir, unrestricted),
		miniagent.EditFileTool(cfg.workdir, unrestricted),
		miniagent.ShellTool(cfg.workdir, unrestricted, cfg.blockedPatterns),
	}
}

// initHistory 在 stateDir+chatID 都非空时打开 history；无状态模式返回 nil。
func initHistory(stateDir, chatID string, logger *slog.Logger) *miniagent.History {
	if stateDir == "" || chatID == "" {
		return nil
	}
	history, err := miniagent.NewHistory(stateDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init history: %v\n", err)
		os.Exit(1)
	}
	return history
}
