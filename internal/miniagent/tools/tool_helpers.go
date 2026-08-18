// tool_helpers.go: helpers dedicated to tool construction/execution (used only by tool_*.go). Originally in the
// core tools.go, moved here to fix the physical misplacement of "core containing tool-specific helpers", paving
// the way for tool sub-packaging (library-ization 5.0.0). Logically belongs to the tool side, not the core loop;
// co-located in the same package only for historical physical layout, with no logical coupling (uses only public
// types + this group of helpers).

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// denyResult builds a pre-execution rejection: no command ran, so ExitCode must be ExitCodeNotSet —
// the zero value 0 would be misread by the event layer as success (P3-4).
func denyResult(format string, a ...any) miniagent.ToolResult {
	return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: fmt.Sprintf(format, a...)}
}

// resolveToolPath resolves a tool path: returns p unchanged when workspaceRoot is empty or p is already absolute;
// otherwise join(workspaceRoot, p) (join includes Clean, but ../ escaping upwards may resolve outside workdir).
// free mode has **no path boundary constraint**: both ../ and absolute paths can escape workdir; isolation is
// guaranteed by the caller (container/low-privilege user) (README §Execution Isolation). openNoFollow only rejects
// the final symlink component and does not constitute a boundary; the file size cap is unrelated to the boundary.
func resolveToolPath(workspaceRoot, p string) string {
	if workspaceRoot == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workspaceRoot, p)
}

// resolveConfinedPath checks that p (relative to root or absolute) stays within root's subtree.
// Returns the absolute resolved path and nil on success, or "" and an error if it escapes.
// When readOnly is false (write tools), targeting the workdir root itself is rejected
// (would destroy the entire workdir); read-only tools may target the root (e.g. listing it).
// All paths reject <root>/.git/** (default mode): direct file access to .git bypasses the git tool's
// allow-list (write .git/hooks/* → hook execution via git commit/pull; edit .git/config remote →
// push exfiltration). Not a security boundary — npm run et al. remain equivalent-shell channels.
func resolveConfinedPath(root, p string, readOnly bool) (string, error) {
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(root, p)
	}
	absTarget, err := filepath.Abs(filepath.Clean(full))
	if err != nil {
		return "", fmt.Errorf("resolve path %q failed: %w", p, err)
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve workdir %q failed: %w", root, err)
	}
	sep := string(filepath.Separator)
	if !readOnly && absTarget == rootAbs {
		return "", fmt.Errorf("path %q points to the workdir root itself, cannot overwrite", p)
	}
	if !strings.HasPrefix(absTarget+sep, rootAbs+sep) {
		return "", fmt.Errorf("path %q escapes workdir (default mode)", p)
	}
	if rel, err := filepath.Rel(rootAbs, absTarget); err == nil && dotGitWithinRoot(rel) {
		return "", fmt.Errorf("path %q targets the .git directory (default mode); use the git tool instead", p)
	}
	return absTarget, nil
}

// dotGitWithinRoot reports whether rel (relative to workdir root) is or is under a ".git" directory
// at any depth — covers ".git", ".git/config", and nested "sub/.git/HEAD" (submodule/worktree layouts).
// Compared case-insensitively: on Windows/macOS filesystems .GIT IS the gitdir (git opens .git
// case-insensitively there), so an exact compare would let .GIT/hooks bypass the guard. Windows 8.3
// short names (GIT~1) remain a known accepted gap.
func dotGitWithinRoot(rel string) bool {
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

// rtkBin caches the lookup of the rtk output-compacting proxy ("" = not deployed). rtk is optional:
// when present, git/go/npm commands route through it for token-compact output; otherwise they exec natively.
// withRtkBin (rtk_wrap_test.go) swaps the cached lookup in tests.
var rtkBin = sync.OnceValue(func() string {
	if p, err := exec.LookPath("rtk"); err == nil {
		return p
	}
	return ""
})

// rtkWrap returns ("rtk", prefix+args) when rtk is deployed, else (bin, prefix[1:]+args).
// prefix is the full native argv head — prefix[0] is bin itself, so the no-rtk fallback keeps
// every argument (dropping prefix, as an earlier version did, turned `git status` into a bare
// `git` usage dump on rtk-less hosts). The caller decides which subcommands are worth proxying
// (rtk covers only a subset per tool).
func rtkWrap(bin string, prefix, args []string) (string, []string) {
	if rtkBin() == "" {
		out := append([]string{}, prefix[1:]...)
		return bin, append(out, args...)
	}
	return "rtk", append(append([]string{}, prefix...), args...)
}

// decodeStrict unmarshals a tool-args JSON object rejecting unknown fields: a field-name typo
// (`{"subcommand":"add","command":"x"}`) used to fall through to EMPTY args, silently turning
// `git add` into a whole-repo stage. The error names the offending key so the LLM self-corrects.
// Trailing data after the object is likewise rejected: providers emit duplicated/concatenated
// payloads under retry/fragmentation, and silently keeping only the first object would make the
// LLM believe both invocations executed.
func decodeStrict(args string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing data after JSON object")
	}
	return nil
}
