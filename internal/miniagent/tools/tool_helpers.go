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
//   - backslash+newline (LF or CRLF) is a POSIX line continuation: both chars are removed, gluing
//     the surrounding text into one word (no token ever carries an embedded newline)
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
			if open == '\'' {
				hasWord = true
				buf = append(buf, r) // single quotes: no escapes
				continue
			}
			if i+1 < len(runes) {
				// POSIX line continuation: backslash + newline (LF, or CRLF) is REMOVED — no character
				// emitted, no word break. An LLM formatting a long args string writes `... \<LF>--flag`,
				// which used to become a token starting with the raw newline: corrupted argv plus the
				// deny matcher (inspects only dash-prefixed tokens) never saw what sh parses as an option.
				if runes[i+1] == '\n' || (runes[i+1] == '\r' && i+2 < len(runes) && runes[i+2] == '\n') {
					// consume the line terminator: i lands ON the '\n' (LF) or after it (CRLF),
					// the loop's i++ then moves past it — no character emitted, no word break.
					if runes[i+1] == '\r' {
						i += 2
					} else {
						i++
					}
					continue
				}
				i++
				hasWord = true
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

// stripDupSubcommand drops a leading args token that merely repeats the subcommand.
// Models habitually emit {"subcommand":"test","args":"test -race ./..."}; assembled argv would
// become `go test test` → "package test is not in std" / `git add` + repeated "add" → pathspec
// error — both look like real failures and send the model into a retry loop it cannot escape
// (observed: 29 of 34 go calls in one session carried the duplicated form). Stripping is safe:
// the repeated token is redundant by construction, and a file genuinely named like the
// subcommand stays reachable when the token appears in its real position.
func stripDupSubcommand(sub string, fields []string) []string {
	if len(fields) > 0 && fields[0] == sub {
		return fields[1:]
	}
	return fields
}

// denyShellMetachars formats the shared rejection for git/go/npm/lint: names the offending
// operator and tells the model the two ways out (single command per call, or the shell tool
// in auto mode).
func denyShellMetachars(rawArgs string) string {
	op := checkShellMetachars(rawArgs)
	return fmt.Sprintf("args contain unquoted shell operator %q — this tool execs argv directly with NO shell, so it would be passed as a literal argument; issue ONE command per call (quote values containing special chars), or use the shell tool (auto mode)", op)
}

// shellMetachars are the unquoted shell operators that models slip into args expecting a shell
// on the other side (`test ./... 2>&1 | head -100` → go receives flag "-100"; `rm -f x && add y`
// → git receives pathspec "rm"). git/go/npm/lint pass argv straight to exec — there is NO shell —
// so these tokens reach the tool as literal arguments and surface as baffling flag errors the
// model cannot diagnose. Rejected up front with a pointer to the right channel instead.
// Detected on the RAW args string only when the metachar sits outside quotes: splitArgsE has
// already dropped quote characters, so post-split detection would flag a legitimately quoted
// `grep -e 'a|b'` pattern. Backslash-escaped metachars are allowed (consistent with splitArgs).
const shellMetachars = "|&;<>`"

// checkShellMetachars reports the first unquoted shell metacharacter in the raw args string.
// Empty result = pass. Line continuations (backslash+newline) were consumed by splitArgs and
// never carry operators here; a trailing backslash is dropped by splitArgs and ignored too.
func checkShellMetachars(rawArgs string) string {
	open := byte(0)
	s := []byte(rawArgs)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && open != '\'':
			i++ // skip the escaped char (and a stray trailing backslash harmlessly)
		case c == '\'' || c == '"':
			switch open {
			case 0:
				open = c
			case c:
				open = 0
			}
		case open == 0 && strings.IndexByte(shellMetachars, c) >= 0:
			if c == '&' && i+1 < len(s) && s[i+1] == '&' {
				return "&&"
			}
			return string(c)
		}
	}
	return ""
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
