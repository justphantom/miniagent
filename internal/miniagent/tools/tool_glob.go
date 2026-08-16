package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/text"
)

const maxGlobEntries = 500

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// GlobTool recursively lists file paths matching a glob, one path per line relative to workdir.
// Uses filepath.Match globs (*, ?, [...]); they do not cross / and do not support ** — for recursion use grep or shell.
// timeout<=0 uses the default fileOpTimeout.
func GlobTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "glob",
		Description: "Recursively lists file paths matching a glob pattern, one per line (relative to workdir). Uses filepath.Match globs (* ? [...]; does not cross /, no **). Excludes .git. Match limit " + strconv.Itoa(maxGlobEntries) + ".",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "A filepath.Match glob pattern, e.g. *.go or *_test.go"},
			"path":    map[string]any{"type": "string", "description": "Root directory, relative to workdir or absolute, defaults to workdir"},
		}, "pattern"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "glob", func(rctx context.Context) miniagent.ToolResult {
				return runGlob(rctx, workspaceRoot, args, maxOutputChars)
			})
		},
	}
}

func runGlob(ctx context.Context, workspaceRoot, args string, maxOutputChars int) miniagent.ToolResult {
	var a globArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v (received %q)", err, args)}
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: pattern"}
	}
	// Two layers: (1) a structural scan for unclosed brackets / trailing lonely backslash —
	// filepath.Match reports syntax errors lazily, only while a chunk still matches the probe, so a
	// malformed tail after a literal prefix (`q*a[`) short-circuits EVERY fabricated probe (verified:
	// all-x probes of length 1..255 return nil error) and the typo silently yields "no matches".
	// (2) the two-name probes catch the shapes Match does report for any input (empty class, `[a-`,
	// nested-bracket-adjacent forms). Shapes Go treats as literals (`x]y`, `[[]]`) stay legal.
	if globPatternMalformed(a.Pattern) {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid glob pattern: %q (unclosed bracket or trailing backslash)", a.Pattern)}
	}
	if _, err := filepath.Match(a.Pattern, "x"); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid glob pattern: %v", err)}
	}
	if _, err := filepath.Match(a.Pattern, "xxxxxxxxxx"); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid glob pattern: %v", err)}
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	var paths []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if path == root {
				return err // root inaccessible: propagate the real error
			}
			return nil //nolint:nilerr // skip inaccessible subtrees, keep accessible parts (consistent with grep)
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Type()&fs.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		ok, _ := filepath.Match(a.Pattern, d.Name())
		if !ok {
			return nil
		}
		if len(paths) >= maxGlobEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		paths = append(paths, rel)
		return nil
	})
	if walkErr != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("glob %q failed: %v", a.Path, walkErr)}
	}
	if len(paths) == 0 {
		return miniagent.ToolResult{Output: "no matches"}
	}
	out := text.Truncate(strings.Join(paths, "\n"), maxOutputChars, "…[glob output truncated]")
	if truncated {
		out += fmt.Sprintf("\n…(over %d entries, collection stopped)", maxGlobEntries)
	}
	return miniagent.ToolResult{Output: out}
}

// globPatternMalformed reports the bracket/backslash structural errors that fabricated-name probes
// cannot reach: a probe fails the leading literal chunk of `q*a[` before the parser ever reaches the
// unclosed '[', so filepath.Match returns nil error for every probe length (measured 1..255). The
// scan walks classes the way Match's parser does — ']' outside a class and '[' inside one are
// literals (verified against filepath.Match), '\' escapes exactly one byte everywhere — and flags an
// unclosed class or a trailing lonely escape, both of which are ErrBadPattern for any input.
func globPatternMalformed(p string) bool {
	i := 0
	for i < len(p) {
		switch p[i] {
		case '\\':
			if i+1 >= len(p) {
				return true // trailing escape with nothing to escape
			}
			i += 2
		case '[':
			j := i + 1
			closed := false
			for j < len(p) {
				if p[j] == '\\' {
					if j+1 >= len(p) {
						return true
					}
					j += 2
					continue
				}
				if p[j] == ']' {
					closed = true
					j++
					break
				}
				j++
			}
			if !closed {
				return true
			}
			i = j
		default:
			i++
		}
	}
	return false
}
