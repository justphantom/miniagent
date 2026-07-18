package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// formatFactsForCLI renders chat-scope facts as an appended system-prompt block.
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
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func runShowCurrent(stateDir, chatID, defaultModel string) {
	if stateDir == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --state-dir is required for --show-current")
		os.Exit(1)
	}
	if chatID == "" {
		fmt.Fprintln(os.Stderr, "miniagent: --chat-id is required for --show-current")
		os.Exit(1)
	}
	h, err := miniagent.NewHistory(stateDir, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init history: %v\n", err)
		os.Exit(1)
	}
	meta, err := miniagent.NewMetaStore(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init meta: %v\n", err)
		os.Exit(1)
	}
	info := struct {
		ChatID     string `json:"chat_id"`
		SessionID  string `json:"session_id"`
		Model      string `json:"model"`
		Directory  string `json:"directory"`
		Permission string `json:"permission"`
	}{
		ChatID:     chatID,
		SessionID:  h.Current(chatID),
		Model:      meta.Model(chatID),
		Directory:  meta.Directory(chatID),
		Permission: meta.Permission(chatID),
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
