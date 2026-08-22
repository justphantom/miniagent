package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/text"
)

const (
	maxGrepMatches   = 500
	maxGrepFileBytes = 50 << 20 // per-file size cap: a huge file (logs/build artifacts) scanned line by line only times out at fileOpTimeout, wasting IO, so the entry-point Stat skips it directly
	// maxGrepRegexNodes limits regex complexity: an AST node count over the limit is rejected outright, preventing a constructed slow regex from burning CPU.
	maxGrepRegexNodes = 100
)

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`
}

// GrepTool recursively regex-searches text files, outputting file:lineno:line (consistent with grep -n).
// When workspaceRoot is empty and path is omitted, it searches the caller process's cwd.
// timeout<=0 uses the default fileOpTimeout.
func GrepTool(workspaceRoot string, timeout time.Duration, maxMatches, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxMatches <= 0 {
		maxMatches = maxGrepMatches
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "grep",
		Description: "Recursively regex-searches text file contents. Outputs path:lineno:line (matches grep -n, for easy locating). Searches workdir by default; use glob to filter by file name (matches base name only, * does not cross /, so sub/*.go won't match). Match limit " + strconv.Itoa(maxMatches) + " lines; output is truncated over " + strconv.Itoa(maxOutputChars) + " characters. Skips .git, binary files, and files over 50MB.",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "A regular expression (Go regexp syntax, e.g. foo, Foo.*, (?i)error)"},
			"path":    map[string]any{"type": "string", "description": "Search root directory, relative to workdir or absolute, defaults to workdir"},
			"glob":    map[string]any{"type": "string", "description": "File-name include filter, a filepath.Match glob (e.g. *.go)"},
		}, "pattern"),
		ResultLimit:   miniagent.MaxToolResultInHistory,
		SplitTruncate: true, // the match-limit/no-match summary is at the tail; front-truncation would lose it
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "search", func(rctx context.Context) miniagent.ToolResult {
				return runGrep(rctx, workspaceRoot, args, maxMatches, maxOutputChars)
			})
		},
	}
}

func runGrep(ctx context.Context, workspaceRoot, args string, maxMatches, maxOutputChars int) miniagent.ToolResult {
	a, err := parseGrepArgs(args)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	if err := validateGrepPattern(a.Pattern); err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid regex: %v", err)}
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	var globFn func(string) bool
	if strings.TrimSpace(a.Glob) != "" {
		a.Glob = strings.TrimSpace(a.Glob)
		// Two-probe validation: filepath.Match on a bogus name returns false but no error if the
		// pattern is syntactically valid (e.g. "*.go" doesn't match "glob-probe"), so we need a
		// second probe that would match a valid pattern — a literal match probes the pattern for
		// syntax errors (aligned with runGlob's globPatternMalformed + probe).
		if _, err := filepath.Match(a.Glob, "probe"); err != nil {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid glob: %v", err)}
		}
		// Separator-bounded probe: a valid glob like "*.go" matches "x.go" but not "probe"; a
		// malformed glob like "[a-" fails on both.
		if _, err := filepath.Match(a.Glob, "x.go"); err != nil {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid glob: %v", err)}
		}
		globFn = func(name string) bool {
			ok, _ := filepath.Match(a.Glob, name)
			return ok
		}
	}
	matches, truncated, err := grepWalk(ctx, root, re, globFn, maxMatches)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("search %q failed: %v", a.Path, err)}
	}
	if len(matches) == 0 {
		return miniagent.ToolResult{Output: "no matches"}
	}
	var sb strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&sb, "%s:%d:%s\n", m.file, m.line, m.text)
	}
	out := text.Truncate(sb.String(), maxOutputChars, "…[grep output truncated]")
	if truncated {
		out += fmt.Sprintf("\n…(over %d matching lines, collection stopped)", maxMatches)
	}
	return miniagent.ToolResult{Output: out}
}

// validateGrepPattern parses the pattern with regexp/syntax and limits the AST node count, preventing a constructed slow regex.
func validateGrepPattern(pattern string) error {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return fmt.Errorf("regex parsing failed: %w", err)
	}
	if n := countRegexNodes(re); n > maxGrepRegexNodes {
		return fmt.Errorf("regex too complex: %d nodes (limit %d)", n, maxGrepRegexNodes)
	}
	return nil
}

func countRegexNodes(re *syntax.Regexp) int {
	if re == nil {
		return 0
	}
	n := 1
	for _, sub := range re.Sub {
		n += countRegexNodes(sub)
	}
	return n
}

func parseGrepArgs(args string) (grepArgs, error) {
	var a grepArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return grepArgs{}, fmt.Errorf("argument parsing failed: %w (received %q)", err, args)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return grepArgs{}, errors.New("missing argument: pattern")
	}
	return a, nil
}

type grepMatch struct {
	file string
	line int
	text string
}

// grepWalk walks root and matches each text file line by line. Inaccessible subtrees/files are skipped rather than failing
// the whole walk (partial readability is still useful). Skips .git, symlinks (to avoid recursing into them by mistake), and
// non-regular files (FIFO/device/socket: os.Open would block, consistent with the read/edit IsRegular guard).
func grepWalk(ctx context.Context, root string, re *regexp.Regexp, globFn func(string) bool, maxMatches int) ([]grepMatch, bool, error) {
	var matches []grepMatch
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == root {
				return walkErr // root missing/inaccessible: a typo'd path must not masquerade as "no matches" (aligned with glob)
			}
			return nil //nolint:nilerr // skip inaccessible subtrees, keep results from accessible parts
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
		// FIFO/device/socket: os.Open would block (a FIFO with no writer hangs forever until fileOpTimeout and leaks an OS thread);
		// skipped consistently with the read/edit IsRegular guard.
		if !d.Type().IsRegular() {
			return nil
		}
		if globFn != nil && !globFn(d.Name()) {
			return nil
		}
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		// A grepFile error (binary/too-large/ErrTooLong etc.) only skips the rest of that file; ms still contains partial matches collected before ErrTooLong and remains usable.
		// Always call grepFile even at cap: only a match found BEYOND the cap proves truncation — a later
		// merely-walkable file must not flag it (exactly maxMatches matches + more files is a complete result).
		ms, _ := grepFile(path, rel, re, maxMatches)
		for _, m := range ms {
			if len(matches) >= maxMatches {
				truncated = true
				return filepath.SkipAll
			}
			matches = append(matches, m)
		}
		return nil
	})
	return matches, truncated, err
}

// grepFile reads path and matches it line by line; display is the file name used in the output (usually the path relative to root).
// Containing NUL is treated as binary and skipped (consistent with the read tool, to prevent garbled output polluting context).
// The entry-point Stat caps per-file size: a no-match huge file (logs/build artifacts) would be scanned line by line until fileOpTimeout, pure IO waste.
func grepFile(path, display string, re *regexp.Regexp, maxMatches int) ([]grepMatch, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Re-check IsRegular: os.Stat follows symlinks, catching a swap-race after the walk-level d.Type() snapshot (path swapped to a FIFO/device/socket),
	// aligned with the read/edit open-level guard (M3-3 gap R4-5).
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not regular, skipped")
	}
	if fi.Size() > maxGrepFileBytes {
		return nil, errors.New("file too large, skipped")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// NewReaderSize, not NewReader: the default 4096 buffer caps Peek, so NULs in bytes 4096..8191
	// escaped the sniff and the "binary" file was searched line by line.
	br := bufio.NewReaderSize(f, 8192)
	head, _ := br.Peek(8192)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, errors.New("binary")
	}
	scanner := bufio.NewScanner(br)
	// The default 64KB line-length cap is too small (compressed/generated files often have very long lines), so loosen it to 1MB.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var matches []grepMatch
	lineno := 0
	for scanner.Scan() {
		lineno++
		if re.MatchString(scanner.Text()) {
			matches = append(matches, grepMatch{file: display, line: lineno, text: scanner.Text()})
			// Per-file match cap: prevents a giant file (every line matches) from transiently allocating a full-file match slice before grepWalk's global maxMatches truncation.
			if len(matches) >= maxMatches {
				return matches, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// ErrTooLong (>1MB single line) etc.: return the partial matches collected so far rather than discarding them; the caller can still use them.
		return matches, err
	}
	return matches, nil
}
