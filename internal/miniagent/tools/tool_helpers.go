// tool_helpers.go: helpers dedicated to tool construction/execution (used only by tool_*.go). Originally in the
// core tools.go, moved here to fix the physical misplacement of "core containing tool-specific helpers", paving
// the way for tool sub-packaging (library-ization 5.0.0). Logically belongs to the tool side, not the core loop;
// co-located in the same package only for historical physical layout, with no logical coupling (uses only public
// types + this group of helpers).

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
func dotGitWithinRoot(rel string) bool {
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == ".git" {
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
func decodeStrict(args string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// splitArgs tokenizes an argument string the way a shell would split words, so that quoted values
// containing spaces survive intact (e.g. `-m "feat: add thing"` -> ["-m","feat: add thing"]).
//
// Semantics (deliberately POSIX-ish but minimal — the git/go/npm/lint tools pass argv directly to
// exec, so this is the ONLY quoting layer; previously strings.Fields split every word apart and
// multi-word commit messages / grep patterns broke):
//   - unquoted whitespace (space/tab/CR/LF) separates words
//   - 'single quotes' preserve everything literally, no escapes
//   - "double quotes" preserve spaces; backslash escapes only " and \ (other chars stay literal)
//   - outside quotes a backslash escapes the next char (so -m one\ word == -m "one word")
//   - quotes are stripped; adjacent segments concatenate (a"b c"d -> ab cd)
//
// splitArgsStrict additionally reports an unterminated quote instead of silently gluing the
// remainder into one word: `--grep=won't --oneline` used to collapse into a single bogus token
// (apostrophe dropped, later flags swallowed) — a hard error points the LLM at the quoting fix.
func splitArgs(s string) []string {
	fields, _ := splitArgsE(s)
	return fields
}

func splitArgsStrict(s string) ([]string, string) {
	return splitArgsE(s)
}

func splitArgsE(s string) (fields []string, unterminated string) {
	var out []string
	var buf []rune
	open := byte(0)  // 0 = none, '\'' or '"' = currently open quote kind
	hasWord := false // a word is "active" once we have buffered a char or entered a quote (so "" makes an empty word)
	flush := func() {
		if hasWord {
			out = append(out, string(buf))
			buf = buf[:0]
			hasWord = false
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '\'':
			hasWord = true
			switch open {
			case 0:
				open = '\''
			case '\'':
				open = 0
			default:
				buf = append(buf, r) // inside double quotes: literal
			}
		case '"':
			hasWord = true
			switch open {
			case 0:
				open = '"'
			case '"':
				open = 0
			default:
				buf = append(buf, r) // inside single quotes: literal
			}
		case '\\':
			hasWord = true
			if open == '\'' {
				buf = append(buf, r) // single quotes: no escapes
				continue
			}
			if i+1 < len(runes) {
				i++
				if open == '"' && runes[i] != '"' && runes[i] != '\\' {
					buf = append(buf, '\\') // inside double quotes backslash escapes only " and \; other chars stay literal
				}
				buf = append(buf, runes[i])
			} // trailing backslash: dropped, no panic
		case ' ', '\t', '\r', '\n':
			if open == 0 {
				flush()
			} else {
				buf = append(buf, r)
			}
		default:
			hasWord = true
			buf = append(buf, r)
		}
	}
	flush()
	if open != 0 {
		u := fmt.Sprintf("has an unterminated %c quote — quote the whole value (e.g. args: \"-m 'feat: won''t break'\") or drop the stray %c; got: %s", open, open, s)
		return out, u
	}
	return out, ""
}

// maxFileResultInHistory is the character cap for results of code-content tools like read/edit entering history:
// code truncation means losing accuracy, so a higher quota than the default policy.MaxToolResultInHistory is given
// (still constrained by read's own maxReadFileChars output cap). miniagent.Tool.ResultLimit takes this value.
const maxFileResultInHistory = 8000

// object builds a JSON Schema object description. When required is empty the key is omitted: the JSON Schema
// spec states that omitting required is equivalent to an empty array, which all compliant backends accept;
// writing a nil slice into the map would serialize as "required":null, triggering a 400 from strict backends
// (e.g. OpenAI).
func object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// runWithTimeout wraps "ctx cancellation check + WithTimeout + goroutine + select fallback" into a single helper,
// reused by file-type tools like read/write/edit/grep/glob (previously 5 near-verbatim boilerplate copies).
// label goes into the timeout/cancellation message (e.g. "read", "search"). fn receives runCtx (with timeout) and
// can check runCtx during long operations/traversals to return early — but Go cannot forcibly terminate a goroutine:
// single-file syscalls (read/write/edit) stuck in D-state are uninterruptible (OS-level limitation); only the
// WalkDir traversals of grep/glob can be terminated promptly via runCtx. fn must return promptly, otherwise
// after the select fallback the goroutine still runs until fn ends naturally (done buffered=1 guarantees the send
// won't block, but does not guarantee fn is interruptible).
func runWithTimeout(ctx context.Context, timeout time.Duration, label string, fn func(ctx context.Context) miniagent.ToolResult) miniagent.ToolResult {
	if err := ctx.Err(); err != nil {
		return miniagent.ToolResult{IsError: true, Output: "cancelled: " + err.Error()}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan miniagent.ToolResult, 1)
	// self-recover inside the goroutine: fn runs in this goroutine, so the caller's safeCall recover cannot catch it —
	// symmetric with safeCall (loop_tools.go)/callLLMOnce; a panic inside file tools is converted to an IsError result
	// instead of crashing the process.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: label + " internal error"}
			}
		}()
		done <- fn(runCtx)
	}()
	select {
	case r := <-done:
		return r
	case <-runCtx.Done():
		// Timeout message carries the duration (the LLM cannot infer it from ctx error strings), and distinguishes
		// the two causes: a parent cancellation surfaces ctx.Err(), a tool's own timeout names the duration — the LLM
		// can then decide "split the command / narrow the test set" instead of mistaking it for a command failure.
		if ctx.Err() != nil {
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: label + " cancelled: " + ctx.Err().Error()}
		}
		// Grace window: fn's kill path (runLimitedOutput closes the pipe on ctx.Done) unblocks the read
		// loop almost immediately — the partial output captured so far (last test names, progress) is the
		// only clue to WHERE it hung, so prefer a late result with body over an instant bare timeout line.
		// Hard-cancel (SIGINT) keeps the fast path: parent ctx above already returned.
		select {
		case r := <-done:
			if r.IsError && strings.Contains(r.Output, "timed out") {
				return r
			}
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet,
				Output: fmt.Sprintf("%s timed out after %s — partial output follows\n%s", label, timeout, r.Output)}
		case <-time.After(2 * time.Second):
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: fmt.Sprintf("%s timed out after %s — narrow the scope (fewer packages / smaller command) and retry", label, timeout)}
		}
	}
}
