package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// confineWrap wraps a tool's Call: before execution it checks that args.path falls within the root subtree; rejects out-of-bounds.
// Used for read/write/edit/grep/glob — read/write/edit require path, grep/glob path is optional
// (defaults to workdir, already constrained by workspaceRoot).
//
// It performs the out-of-bounds check only when args can be parsed into a non-empty path; when path is
// absent/empty or JSON is invalid it passes through to orig: each tool validates path itself (write/edit
// errors on empty path; read requires it; grep/glob with empty path falls back to workspaceRoot). Previously
// rejecting all empty paths would incorrectly break grep/glob (path is optional), making default mode essentially unusable.
//
// TOCTOU trade-off (review P2-11): checkConfine is pure lexical validation (Clean+Abs+HasPrefix), with a
// window between it and the subsequent MkdirAll/Rename; under runToolsParallel parallel execution, shell can
// replace a parent directory with a symlink within the window, making the final rename land outside workdir.
// default mode is not a security boundary in the first place (shell is already an unrestricted write primitive,
// as stated in the README), so capability is not increased — this is only a guardrail against misoperation,
// without forcibly calling EvalSymlinks (which would change default semantics and introduce new failure modes).
// True isolation relies on a low-privilege user + container + OS layer (see README "Runtime isolation").
func confineWrap(tool miniagent.Tool, root string) miniagent.Tool {
	orig := tool.Call
	tool.Call = func(ctx context.Context, args string) miniagent.ToolResult {
		var p struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
			if err := checkConfine(root, p.Path); err != nil {
				return miniagent.ToolResult{IsError: true, Output: err.Error()}
			}
		}
		return orig(ctx, args)
	}
	return tool
}

// checkConfine is a lightweight path validation: p (relative to root or absolute) after Clean+Abs must fall within the root subtree.
// It additionally checks that existing path components from root to target are not symlinks, narrowing the TOCTOU window.
// It does not do an EvalSymlinks follow-up — default is a thin soft constraint, symlink escapes are mitigated by the caller's OS isolation.
func checkConfine(root, p string) error {
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(root, p)
	}
	absTarget, err := filepath.Abs(filepath.Clean(full))
	if err != nil {
		return fmt.Errorf("resolve path %q failed: %w", p, err)
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return fmt.Errorf("resolve workdir %q failed: %w", root, err)
	}
	sep := string(filepath.Separator)
	// Reject path="." or path equal to the workdir absolute path: rename over a directory would EISDIR (ambiguous error),
	// and if MkdirAll/Rename actually took effect it would destroy the entire workdir (review P3-8).
	if absTarget == rootAbs {
		return fmt.Errorf("path %q points to the workdir root itself, cannot overwrite", p)
	}
	if !strings.HasPrefix(absTarget+sep, rootAbs+sep) {
		return fmt.Errorf("path %q escapes workdir (default mode)", p)
	}
	// Check whether existing path components contain symlinks: an attacker may replace some directory level with a symlink
	// to make the final IO land outside workdir. This check runs before IO and narrows but cannot fully eliminate TOCTOU.
	rel, err := filepath.Rel(rootAbs, absTarget)
	if err != nil {
		return fmt.Errorf("resolve path %q relative to workdir failed: %w", p, err)
	}
	current := rootAbs
	for part := range strings.SplitSeq(rel, sep) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // remaining components do not exist, will be created by subsequent MkdirAll/Create
			}
			return fmt.Errorf("check path %q failed: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q contains symlink %q (default mode)", p, current)
		}
	}
	return nil
}
