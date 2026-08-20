package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/text"
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
	content, truncated, err := readFileContent(full, maxBytes)
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
	out := text.Truncate(formatted, maxBytes/4, "…")
	// Cap marker goes at the very END and AFTER the char truncate (else the char cap can cut it off):
	// without it the byte cap is silent — the model cannot distinguish "file ends here" from
	// "output was cut", and would build edit old_strings from lines that only look complete.
	// offset/limit paging already narrows the view, so the marker names the WINDOW, not the byte count.
	if truncated {
		if a.Offset > 1 || a.Limit > 0 {
			out += fmt.Sprintf("\n…[file exceeds the %d-byte read cap; this page shows only part of the first %d bytes]", maxBytes, maxBytes)
		} else {
			out += fmt.Sprintf("\n…[file exceeds the %d-byte read cap; showing the first %d bytes]", maxBytes, maxBytes)
		}
	}
	return miniagent.ToolResult{Output: out}
}

func parseReadArgs(args string) (readFileArgs, error) {
	var a readFileArgs
	if err := decodeStrict(args, &a); err != nil {
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

// readFileContent reads at most maxBytes and reports whether the file is larger (maxBytes+1 probe):
// truncation must be DETECTED, not silently applied — the caller drops the trailing partial line
// and appends a visible marker, so the last rendered line is never a mid-line fragment the model
// would otherwise copy into edit old_strings.
func readFileContent(full string, maxBytes int) (string, bool, error) {
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return "", false, err
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
		// Drop the trailing partial line: the cut is a byte cut, so the tail after the last '\n'
		// is a fragment of a real line, not a line.
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
		} else {
			data = nil // single line longer than the cap: keep nothing rather than a fragment
		}
	}
	return string(data), truncated, nil
}

// formatLines prefixes content with "N │ line" and extracts the range [offset, offset+limit-1].
// offset<=0 is treated as 1; limit<=0 or out of range is treated as read-to-end.
// When offset exceeds the file's line count, returns an error (so the caller marks IsError, rather than silently
// emitting empty output).
// Empty file (content=="") returns an empty string directly, avoiding a spurious empty line "1 │ ".
func formatLines(content string, offset, limit int) (string, error) {
	if content == "" {
		return "", nil
	}
	// Explicit limit>maxLineLimit is a hard error, not a silent clamp: a clamped read returns fewer
	// lines than limit=0 (read-to-end) would, with no marker — the model concludes the file ends there.
	if limit > maxLineLimit {
		return "", fmt.Errorf("limit %d exceeds the maximum of %d lines per read", limit, maxLineLimit)
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
