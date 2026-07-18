package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// formatFactsForCLI renders chat-scope facts as an appended system-prompt block.
// 双上限：条数与字符。事实累积过多时截断并标注，避免 system prompt 膨胀
// 挤占模型上下文（该路径不受 maxHistoryTokens 裁剪约束）。
const (
	maxMemoryContextItems = 30
	maxMemoryContextChars = 2000
)

func formatFactsForCLI(facts []miniagent.Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n以下是与当前对话相关的已知事实（由用户或之前的对话沉淀）：\n")
	shown := 0
	truncated := false
	for _, f := range facts {
		line := fmt.Sprintf("- %s: %s\n", f.Key, f.Value)
		if sb.Len()+len(line) > maxMemoryContextChars {
			truncated = true
			break
		}
		sb.WriteString(line)
		shown++
		if shown >= maxMemoryContextItems {
			truncated = true
			break
		}
	}
	if truncated {
		fmt.Fprintf(&sb, "（共 %d 条，已显示前 %d 条）\n", len(facts), shown)
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

func runShowCurrent(stateDir, chatID string) {
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

// toolConfig 是 buildTools 的入参，把 main 的 flag 解析结果集中传递，
// 避免函数签名挂一长串 *string。
type toolConfig struct {
	permission      string
	workdir         string
	blockedPatterns []string
}

// buildTools 按权限模式装配工具集。空 workdir 与模式的组合会打印 stderr
// 告警但仍返回已注册的工具（与历史行为一致）。
func buildTools(cfg toolConfig) []miniagent.Tool {
	unrestricted := cfg.permission == "free"
	var tools []miniagent.Tool
	switch cfg.permission {
	case "plan":
		if cfg.workdir != "" {
			tools = append(tools, miniagent.ReadFileTool(cfg.workdir, unrestricted))
		} else {
			fmt.Fprintln(os.Stderr, "miniagent: --workdir is empty, read_file not registered (plan mode needs a workspace)")
		}
		tools = append(tools, miniagent.WebFetchTool(nil))
	default:
		if cfg.workdir != "" || unrestricted {
			tools = append(tools,
				miniagent.ReadFileTool(cfg.workdir, unrestricted),
				miniagent.WriteFileTool(cfg.workdir, unrestricted),
				miniagent.EditFileTool(cfg.workdir, unrestricted),
				miniagent.ShellTool(cfg.workdir, unrestricted, cfg.blockedPatterns),
			)
		} else {
			fmt.Fprintln(os.Stderr, "miniagent: --workdir is empty AND permission is not free; read_file/write_file/shell/edit_file not registered")
		}
		tools = append(tools, miniagent.WebFetchTool(nil))
	}
	return tools
}

// stores 聚合一次 turn 需要的三个持久化句柄。
type stores struct {
	history *miniagent.History
	facts   *miniagent.FactStore
	meta    *miniagent.MetaStore
}

// initStores 在 stateDir+chatID 都非空时打开三个 store，并把 model/dir/permission
// 写入 meta；返回的 stores 字段任一可能为 nil（无状态模式）。
func initStores(stateDir, chatID, model, workdir, permission string, logger *slog.Logger) stores {
	if stateDir == "" || chatID == "" {
		return stores{}
	}
	history, err := miniagent.NewHistory(stateDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init history: %v\n", err)
		os.Exit(1)
	}
	facts, err := miniagent.NewFactStore(stateDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init memory: %v\n", err)
		os.Exit(1)
	}
	meta, err := miniagent.NewMetaStore(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: init meta: %v\n", err)
		os.Exit(1)
	}
	if err := meta.SetModel(chatID, model); err != nil {
		logger.Warn("meta: set model failed", "error", err)
	}
	if err := meta.SetDirectory(chatID, workdir); err != nil {
		logger.Warn("meta: set directory failed", "error", err)
	}
	if err := meta.SetPermission(chatID, permission); err != nil {
		logger.Warn("meta: set permission failed", "error", err)
	}
	return stores{history: history, facts: facts, meta: meta}
}
