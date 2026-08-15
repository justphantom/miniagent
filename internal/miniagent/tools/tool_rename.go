package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"path/filepath"
	"time"
)

type renameArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func RenameTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return miniagent.Tool{
		Name:        "rename",
		Description: "Rename or move a file/directory within the workdir subtree. Rejects paths escaping workdir, the workdir root itself, and non-existent sources.",
		Parameters: object(map[string]any{
			"from": map[string]any{"type": "string", "description": "Source path, relative to workdir or absolute"},
			"to":   map[string]any{"type": "string", "description": "Destination path, relative to workdir or absolute"},
		}, "from", "to"),
		ResultLimit: maxFileResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "rename", func(_ context.Context) miniagent.ToolResult {
				return runRename(workspaceRoot, args)
			})
		},
	}
}

func runRename(workspaceRoot, args string) miniagent.ToolResult {
	var a renameArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v", err)}
	}
	if a.From == "" || a.To == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing parameter: from and to are both required"}
	}
	fromFull, err := resolveConfinedPath(workspaceRoot, a.From, false)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	toFull, err := resolveConfinedPath(workspaceRoot, a.To, false)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	if info, err := os.Lstat(fromFull); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("source %q not found: %v", a.From, err)}
	} else if info.Mode()&os.ModeSymlink != 0 {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("source %q is a symlink, refusing to rename", a.From)}
	}
	if err := os.MkdirAll(filepath.Dir(toFull), 0o750); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to create parent directory: %v", err)}
	}
	if err := os.Rename(fromFull, toFull); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("rename %q -> %q failed: %v", a.From, a.To, err)}
	}
	return miniagent.ToolResult{Output: fmt.Sprintf("renamed %s -> %s", a.From, a.To)}
}
