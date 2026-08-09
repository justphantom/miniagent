package tools

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxCodemapEntries   = 500
	defaultCodemapDepth = 3
)

type codemapArgs struct {
	Path  string `json:"path,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

// CodemapTool returns a directory-structure overview as an indented tree (directories annotated with child counts),
// filling the structure-aware gap between glob (flat list) and read (single-file full text).
// Skips .git and symlinks (prevents accidental recursion into them); depth<=0 means unlimited depth (still capped
// by the entry limit). timeout<=0 uses the default fileOpTimeout.
func CodemapTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return miniagent.Tool{
		Name: "codemap",
		Description: "Returns a directory-tree overview: indentation denotes hierarchy (2 spaces per level), directories annotate the direct child count. Skips .git and symlinks. Entry limit " +
			strconv.Itoa(maxCodemapEntries) + ". Use it for a low-cost overview of the repository layout; to view file contents use read, to filter by file name use glob.",
		Parameters: object(map[string]any{
			"path":  map[string]any{"type": "string", "description": "Root directory, relative to workdir or absolute; defaults to workdir"},
			"depth": map[string]any{"type": "integer", "description": "Maximum recursion depth, default " + strconv.Itoa(defaultCodemapDepth) + " (omitted/0 are equivalent); <0 means unlimited (still capped by the entry limit)"},
		}),
		ResultLimit:   miniagent.MaxToolResultInHistory,
		SplitTruncate: true, // the entry-limit hint is at the tail; front-truncation would lose it
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "traverse", func(rctx context.Context) miniagent.ToolResult { return runCodemap(rctx, workspaceRoot, args) })
		},
	}
}

func runCodemap(ctx context.Context, workspaceRoot, args string) miniagent.ToolResult {
	var a codemapArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to parse args: %v (received %q)", err, args)}
	}
	if a.Depth == 0 {
		a.Depth = defaultCodemapDepth
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		if err == nil {
				err = errors.New("not a directory")
		}
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to traverse %q: %v", a.Path, err)}
	}
	lines, truncated, err := codemapWalk(ctx, root, a.Depth)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to traverse %q: %v", a.Path, err)}
	}
	if len(lines) == 0 {
		return miniagent.ToolResult{Output: "(empty directory)"}
	}
	out := strings.Join(lines, "\n")
	if truncated {
		out += fmt.Sprintf("\n… (over %d entries, collection stopped)", maxCodemapEntries)
	}
	return miniagent.ToolResult{Output: out}
}

// codemapWalk walks root producing tree lines. Directory names get a "/" suffix and are annotated with the direct
// child count (.git/symlinks are not counted); directories beyond depth are listed by name only without expanding
// their subtree, and their count is annotated as "?".
// Once the entry count (including directories themselves) reaches maxCodemapEntries it triggers SkipAll.
func codemapWalk(ctx context.Context, root string, maxDepth int) ([]string, bool, error) {
	var lines []string
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil //nolint:nilerr // skip inaccessible subtrees and keep the accessible parts (consistent with grep)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil //nolint:nilerr // Rel only fails for interceptor paths of already-traversed paths; just skip this entry
		}
		if rel == "." {
			return nil
		}
		if len(lines) >= maxCodemapEntries {
			truncated = true
			return filepath.SkipAll
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		indent := strings.Repeat("  ", depth-1)
		if !d.IsDir() {
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			lines = append(lines, indent+d.Name())
			return nil
		}
		if d.Name() == ".git" || d.Type()&fs.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		if maxDepth > 0 && depth >= maxDepth {
			// depth boundary: the subtree is not expanded and the count is unknown; annotate "?" to avoid misreporting.
			lines = append(lines, indent+d.Name()+"/ (? items)")
			return filepath.SkipDir
		}
		n, ok := countDirItems(path)
		if ok {
			lines = append(lines, fmt.Sprintf("%s%s/ (%d items)", indent, d.Name(), n))
		} else {
			lines = append(lines, indent+d.Name()+"/ (? items)")
		}
		return nil
	})
	return lines, truncated, err
}

// countDirItems counts the direct child entries of dir (skipping .git and symlinks, consistent with the walk policy).
// ok=false means the read failed (permissions, etc.): the caller uses this to annotate "? items" instead of falsely
// reporting 0, consistent with the depth-boundary annotation.
func countDirItems(dir string) (int, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, e := range entries {
		if e.Name() == ".git" || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		n++
	}
	return n, true
}
