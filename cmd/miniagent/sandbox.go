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

// confineWrap wraps a tool's Call: before execution it checks that every path field carried by args
// falls within the root subtree; rejects out-of-bounds. Used for read/write/edit/grep/glob/rename/delete —
// read/write/edit/delete carry a single path (required); grep/glob path is optional (defaults to workdir,
// already constrained by workspaceRoot); rename carries {from,to} instead of path.
//
// It performs the out-of-bounds check only on non-empty fields; when path is absent/empty or JSON is
// invalid it passes through to orig: each tool validates path itself (write/edit errors on empty path;
// read requires it; grep/glob with empty path falls back to workspaceRoot). Previously rejecting all
// empty paths would incorrectly break grep/glob (path is optional), making default mode essentially unusable.
// rename's from/to are checked with readOnly=false: both endpoints are write surfaces (MkdirAll on the
// destination parent, os.Rename landing spot) — an unvalidated to is exactly the escape the wrap exists to stop.
//
// TOCTOU trade-off (review P2-11): checkConfine is pure lexical validation (Clean+Abs+HasPrefix), with a
// window between it and the subsequent MkdirAll/Rename; under runToolsParallel parallel execution, shell can
// replace a parent directory with a symlink within the window, making the final rename land outside workdir.
// default mode is not a security boundary in the first place (shell is already an unrestricted write primitive,
// as stated in the README), so capability is not increased — this is only a guardrail against misoperation,
// without forcibly calling EvalSymlinks (which would change default semantics and introduce new failure modes).
// True isolation relies on a low-privilege user + container + OS layer (see README "Runtime isolation").
func confineWrap(tool miniagent.Tool, root string, readOnly bool, allowDirs []string, evalSymlinks ...bool) miniagent.Tool {
	orig := tool.Call
	tool.Call = func(ctx context.Context, args string) miniagent.ToolResult {
		var p struct {
			Path string `json:"path"`
			From string `json:"from"`
			To   string `json:"to"`
		}
		if json.Unmarshal([]byte(args), &p) == nil {
			// rename sends {from,to} rather than {path}; both need the write-side check or rename
			// is a silent no-op pass-through (review: cmd-layer confine did not apply to it).
			for _, target := range []string{p.From, p.To} {
				if target == "" {
					continue
				}
				if err := checkConfine(root, target, false, evalSymlinks...); err != nil {
					return miniagent.ToolResult{IsError: true, Output: err.Error()}
				}
			}
			if p.Path != "" {
				// Read-only tools get an allowDirs exception (§P1-A read-back); write tools pass nil.
				if !readOnly || !pathInRoots(p.Path, allowDirs) {
					if err := checkConfine(root, p.Path, readOnly, evalSymlinks...); err != nil {
						return miniagent.ToolResult{IsError: true, Output: err.Error()}
					}
				}
			}
		}
		return orig(ctx, args)
	}
	return tool
}

// checkConfine is a lightweight path validation: p (relative to root or absolute) after Clean+Abs must fall within the root subtree.
// It additionally checks that existing path components from root to target are not symlinks, narrowing the TOCTOU window.
// It does not do an EvalSymlinks follow-up — default is a thin soft constraint, symlink escapes are mitigated by the caller's OS isolation.
func checkConfine(root, p string, readOnly bool, evalSymlinks ...bool) error {
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
	// Reject path="." or path equal to the workdir absolute path for WRITE tools: rename over a directory would EISDIR
	// (ambiguous error), and if MkdirAll/Rename actually took effect it would destroy the entire workdir (review P3-8).
	// Read-only tools (read/grep/glob) skip this: targeting the whole workdir to read/list is a legitimate operation.
	if !readOnly && absTarget == rootAbs {
		return fmt.Errorf("path %q points to the workdir root itself, cannot overwrite", p)
	}
	if !strings.HasPrefix(absTarget+sep, rootAbs+sep) {
		return fmt.Errorf("path %q escapes workdir (default mode)", p)
	}
	// Reject <root>/.git/** for all tools (read included): direct file access to .git bypasses the git tool's
	// allow-list (write .git/hooks/* → hook execution via git commit/pull; edit .git/config remote → push
	// exfiltration; read .git/config leaks remote URLs/credentials-in-config). Mirrors resolveConfinedPath
	// (tools package) for the rename/delete pair. Guardrail, not a security boundary.
	if rel, err := filepath.Rel(rootAbs, absTarget); err == nil && dotGitWithinRoot(rel) {
		return fmt.Errorf("path %q targets the .git directory (default mode); use the git tool instead", p)
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
	// Optional final EvalSymlinks (confine_eval_symlinks, opt-in): resolves the FULL path to narrow the parallel-symlink-swap
	// TOCTOU window the lexical per-component check above leaves open. Only reached when every component exists (the loop above
	// returned early on a not-yet-created component). Both target and root are resolved so a workdir reached via a symlink does not
	// false-positive (real-root comparison). On a vanished-path race (ENOENT) it falls back to the lexical result — preserves
	// create semantics (EvalSymlinks errors on a non-existent path). Guardrail hardening, not security.
	if eval := len(evalSymlinks) > 0 && evalSymlinks[0]; eval {
		if realTarget, err := filepath.EvalSymlinks(absTarget); err == nil {
			realRoot, rerr := filepath.EvalSymlinks(rootAbs)
			if rerr != nil {
				realRoot = rootAbs // root unreadable: fall back to lexical root (do not harden beyond lexical)
			}
			if !strings.HasPrefix(realTarget+sep, realRoot+sep) {
				return fmt.Errorf("path %q resolves outside workdir after symlink evaluation (confine_eval_symlinks)", p)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("eval symlinks %q failed: %w", p, err)
		}
	}
	return nil
}

// dotGitWithinRoot reports whether rel (relative to workdir root) is or is under a ".git" directory
// at any depth — covers ".git", ".git/config", and nested "sub/.git/HEAD" (submodule/worktree layouts).
// Compared case-insensitively: on Windows/macOS filesystems .GIT IS the gitdir (git opens .git
// case-insensitively there), so an exact compare would let .GIT/hooks bypass the guard. Windows 8.3
// short names (GIT~1) remain a known accepted gap. Duplicated from tools.resolveConfinedPath's helper
// rather than exported: cmd → core must not add a reverse dependency for one predicate (invariant #14;
// the two confine layers already mirror each other).
// pathInRoots reports whether p (absolute, as the §P1-A marker always emits) falls inside any of roots.
// Relative paths resolve against workdir and can never point into an external allow-root, so they return
// false (they stay subject to checkConfine). Roots are cleaned; an empty list never matches. Read-only only.
func pathInRoots(p string, roots []string) bool {
	if len(roots) == 0 || !filepath.IsAbs(p) {
		return false
	}
	abs := filepath.Clean(p)
	sep := string(filepath.Separator)
	for _, r := range roots {
		root := filepath.Clean(r)
		if abs == root || strings.HasPrefix(abs+sep, root+sep) {
			return true
		}
	}
	return false
}

func dotGitWithinRoot(rel string) bool {
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}
