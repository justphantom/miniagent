package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"time"
)

type deleteArgs struct {
	Path string `json:"path"`
}

func DeleteTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return miniagent.Tool{
		Name:        "delete",
		Description: "Delete a file or empty directory within the workdir subtree. Rejects paths escaping workdir, the workdir root, glob patterns, and non-existent targets.",
		Parameters: object(map[string]any{
			"path": map[string]any{"type": "string", "description": "Path of the file or empty directory to delete, relative to workdir or absolute"},
		}, "path"),
		ResultLimit: maxFileResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "delete", func(_ context.Context) miniagent.ToolResult {
				return runDelete(workspaceRoot, args)
			})
		},
	}
}

func runDelete(workspaceRoot, args string) miniagent.ToolResult {
	var a deleteArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v", err)}
	}
	if a.Path == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing parameter: path"}
	}
	if containsGlob(a.Path) {
		return miniagent.ToolResult{IsError: true, Output: "path must not contain glob characters; use individual delete calls"}
	}
	full, err := resolveConfinedPath(workspaceRoot, a.Path, false)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	info, err := os.Lstat(full)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("%q not found: %v", a.Path, err)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("%q is a symlink, refusing to delete", a.Path)}
	}
	if info.IsDir() {
		entries, err := os.ReadDir(full)
		if err != nil {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read directory %q: %v", a.Path, err)}
		}
		if len(entries) > 0 {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("directory %q is not empty (%d entries); delete contents first", a.Path, len(entries))}
		}
	}
	if err := os.Remove(full); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("delete %q failed: %v", a.Path, err)}
	}
	return miniagent.ToolResult{Output: "deleted " + a.Path}
}

// containsGlob reports whether s contains any filepath.Match glob metacharacter.
func containsGlob(s string) bool {
	for _, c := range s {
		switch c {
		case '*', '?', '[', ']':
			return true
		}
	}
	return false
}
