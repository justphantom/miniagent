package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/text"
)

// maxReadFileBytes is the per-file read cap for the read tool: 1MB covers large files
// (generated code, large data constants) while preventing huge logs/generated files from
// exhausting memory. Overridden at runtime via miniagent.Limits.MaxReadFileBytes (<=0 uses this default).
const maxReadFileBytes = 1 << 20 // 1MB

// fileOpTimeout is the built-in operation timeout for read/edit: IsRegular already cuts off
// the main cause of FIFO blocking; this is a fallback for hung filesystems (NFS server down,
// bad disk, network black hole).
//
// Known trade-off (P2-10, documented rather than refactored): after select hits runCtx.Done
// the main flow returns, but the background read goroutine may still block in an uninterruptible
// kernel read (D state, e.g. NFS server unreachable where read ignores signals); ctx cannot
// cancel it -> the goroutine is not reclaimable, accumulating over a long-running process. A
// complete fix requires non-blocking IO + periodic ctx polling (high cost, low benefit; default
// isolation keeps processes short-lived), so this is documented here for now.
// Full avoidance relies on the caller configuring container/low-privilege user per README "Run Isolation".
const fileOpTimeout = 30 * time.Second

const maxLineLimit = 10000

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ReadFileTool returns a read tool bound to workspaceRoot. timeout<=0 uses the default fileOpTimeout.
// maxBytes<=0 uses the default maxReadFileBytes (injected via miniagent.Limits.MaxReadFileBytes).
func ReadFileTool(workspaceRoot string, timeout time.Duration, maxBytes int) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxBytes <= 0 {
		maxBytes = maxReadFileBytes
	}
	maxChars := maxBytes / 4
	return miniagent.Tool{
		Name:        "read",
		Description: fmt.Sprintf("Reads a text file and outputs it with line numbers (N │ line; edit locates offset based on this). Supports offset/limit for paginating large files. Rejects binary files (containing NUL), symlinks as the final path component (intermediate directory symlinks are still followed), and non-regular files. Per-file limit %d bytes, output limit %d chars. path is relative to workdir or absolute.", maxBytes, maxChars),
		Parameters: object(map[string]any{
			"path":   map[string]any{"type": "string", "description": "File path to read, relative to workdir or absolute"},
			"offset": map[string]any{"type": "integer", "description": "Starting line number (1-based), default 1 (from the beginning)"},
			"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to return, default all"},
		}, "path"),
		ResultLimit: maxFileResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "read", func(_ context.Context) miniagent.ToolResult { return runReadFile(workspaceRoot, args, maxBytes) })
		},
	}
}

func runReadFile(workspaceRoot, args string, maxBytes int) miniagent.ToolResult {
	a, err := parseReadArgs(args)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	// Lstat instead of Stat: aligns with edit/openNoFollow, rejecting a symlink as the final
	// path component directly (Stat would follow the symlink to read the target, contradicting
	// the "reject symlinks" description). Intermediate directory symlinks are still followed
	// (read has no path constraints; only the final component is guarded by O_NOFOLLOW/Lstat).
	info, err := os.Lstat(full)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", a.Path, err)}
	}
	if !info.Mode().IsRegular() {
		// Reject non-regular files: FIFO/device/socket would block openNoFollow (a FIFO with no
		// writer hangs forever) or read out non-text byte streams; symlink (mode contains
		// ModeSymlink) is also intercepted here, giving a clear error instead of falling through
		// to openNoFollow's O_NOFOLLOW errno.
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("%q is not a regular file (mode=%s); only regular files are supported", a.Path, info.Mode().String())}
	}
	content, err := readFileContent(full, maxBytes)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", a.Path, err)}
	}
	// Binary fallback: containing NUL bytes is almost certainly binary (UTF-8/common encodings
	// never use NUL); letting the LLM read garbled output would pollute context + waste tokens.
	// Scanning the first 8 KiB is enough for a low-cost interception.
	scanLimit := min(len(content), 8192)
	if strings.IndexByte(content[:scanLimit], 0) >= 0 {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("%q is a binary file (contains NUL bytes); read only supports text", a.Path)}
	}
	// Always include line numbers: edit requires exact matching; line numbers help the LLM locate offset.
	formatted, err := formatLines(content, a.Offset, a.Limit)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	return miniagent.ToolResult{Output: text.Truncate(formatted, maxBytes/4, "…")}
}

func parseReadArgs(args string) (readFileArgs, error) {
	var a readFileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return readFileArgs{}, fmt.Errorf("argument parsing failed: %w (received %q)", err, args)
	}
	if a.Path == "" {
		return readFileArgs{}, errors.New("missing argument: path")
	}
	if a.Offset < 0 {
		a.Offset = 0
	}
	return a, nil
}

// LimitReader naturally handles oversized files: content beyond maxReadFileBytes is discarded;
// no need to pre-branch on file size.
func readFileContent(full string, maxBytes int) (string, error) {
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// formatLines prefixes content with "N │ line" and extracts the range [offset, offset+limit-1].
// offset<=0 is treated as 1; limit<=0 or out of range is treated as read-to-end; limit>maxLineLimit is truncated.
// When offset exceeds the file's line count, returns an error (so the caller marks IsError, rather than silently
// emitting empty output).
// Empty file (content=="") returns an empty string directly, avoiding a spurious empty line "1 │ ".
func formatLines(content string, offset, limit int) (string, error) {
	if content == "" {
		return "", nil
	}
	// limit<=0 (including negatives) means read-to-end (no truncation); only limit>maxLineLimit is truncated. Consistent with doc "limit<=0 ... read to end".
	if limit > maxLineLimit {
		limit = maxLineLimit
	}
	lines := strings.Split(content, "\n")
	// POSIX text files ending with a newline make Split produce a trailing empty string; treating it as a line
	// would emit a spurious trailing empty line (N │ ) that misleads the LLM's line numbers/edits.
	// Drop the trailing empty string only when the original content ends with \n (preserving the real last line when there's no trailing newline).
	if n := len(lines); n > 0 && lines[n-1] == "" && strings.HasSuffix(content, "\n") {
		lines = lines[:n-1]
	}
	start := max(offset, 1)
	end := len(lines)
	if limit > 0 && start+limit-1 < end {
		end = start + limit - 1
	}
	if start > len(lines) {
		return "", fmt.Errorf("offset %d exceeds the file's line count (%d lines total)", start, len(lines))
	}
	var sb strings.Builder
	width := len(strconv.Itoa(end))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%*d │ %s\n", width, i, lines[i-1])
	}
	return sb.String(), nil
}
