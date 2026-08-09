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

const maxWriteFileBytes = 10 << 20

// writeOpTimeout is the built-in operation timeout for write: the atomic write itself is very fast; this is a fallback for extreme filesystem latency.
const writeOpTimeout = 30 * time.Second

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFileTool returns a write tool bound to workspaceRoot. timeout<=0 uses the default writeOpTimeout.
func WriteFileTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = writeOpTimeout
	}
	desc := fmt.Sprintf("Overwrite the entire file (auto-creating parent directories, preserving original file permissions). content limit 10MiB. path is relative to workdir or absolute. Use only for creating new files or full rewrites; for partial changes use edit. Timeout %s.", timeout)
	return miniagent.Tool{
		Name:        "write",
		Description: desc,
		Parameters: object(map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path of the file to write, relative to workdir or absolute"},
			"content": map[string]any{"type": "string", "description": "Complete file content to write"},
		}, "path", "content"),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "write", func(_ context.Context) miniagent.ToolResult { return runWriteFile(workspaceRoot, args) })
		},
	}
}

func runWriteFile(workspaceRoot, args string) miniagent.ToolResult {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to parse args: %v (received %q)", err, args)}
	}
	if a.Path == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing parameter: path"}
	}
	if len(a.Content) > maxWriteFileBytes {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("content exceeds the maximum limit of %d bytes", maxWriteFileBytes)}
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to create parent directory: %v", err)}
	}
	mode := os.FileMode(0o600) // new-file default (owner-only, safer on multi-user hosts); overwrite preserves the existing perm below
	if info, err := os.Lstat(full); err == nil {
		// Reject non-regular files: FIFO/character devices would cause ambiguous errors from
		// subsequent Rename, directories would EISDIR; aligned with edit to report "not a
		// regular file" explicitly (review P3-7).
		if !info.Mode().IsRegular() {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("%q is not a regular file (mode=%s), only regular file is supported", a.Path, info.Mode().String())}
		}
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(a.Content), mode); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to write %q: %v", a.Path, err)}
	}
	return miniagent.ToolResult{Output: fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path)}
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	// Write first then Chmod: CreateTemp defaults to 0600, so the temp keeps the
	// narrowest permissions during the write; only after writing do we set the target
	// perm (0600 for new files, or the preserved perm of an existing file), avoiding
	// exposing unfinished content with wide permissions mid-write.
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	return os.Rename(tmpName, path)
}
