package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"path/filepath"
	"strings"
)

// GuardShell wraps a shell tool's Call with the default-mode allowlist + cd-confine guardrails (opt-in via buildTools).
// It mirrors confineWrap (cmd/miniagent/sandbox.go): parse args, validate, reject or pass through. Kept in the tools
// package — not reusing cmd/miniagent.checkConfine — to respect the cmd→core dependency direction (invariant #14).
//
// Both checks are best-effort LEXICAL guardrails, NOT a security boundary: a shell is a full language, so eval/$()/
// backticks/alias/obfuscation can bypass them, and true isolation still relies on the caller's OS layer (README
// "default mode", invariant #13). They guard against misfired tool calls the same way the sudo/su denylist does.
func GuardShell(tool miniagent.Tool, workdir string, allowlist []string, confineCD bool) miniagent.Tool {
	orig := tool.Call
	tool.Call = func(ctx context.Context, args string) miniagent.ToolResult {
		var a struct {
			Command string `json:"command"`
		}
		// Only validate when a command is present; on missing/invalid JSON pass through to orig (which errors on empty
		// command itself) — consistent with confineWrap's empty-path fallthrough.
		if json.Unmarshal([]byte(args), &a) == nil && a.Command != "" {
			if err := guardCommand(a.Command, confineCD, allowlist, workdir); err != nil {
				return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: err.Error()}
			}
		}
		return orig(ctx, args)
	}
	return tool
}

// guardCommand runs the configured default-mode shell guardrails in order. allowlist is checked only when non-empty;
// cd-confine only when confineCD is set. workdir is assumed absolute (main.go guarantees it) for the cd target check.
func guardCommand(command string, confineCD bool, allowlist []string, workdir string) error {
	if err := checkAllowlist(command, allowlist); err != nil {
		return err
	}
	if confineCD {
		if err := checkConfineCD(command, workdir); err != nil {
			return err
		}
	}
	return nil
}

// checkAllowlist rejects any simple command whose name is not in allow. Every command in a pipeline/list
// (a | b && c; d) is checked independently. An empty allow list disables the check (passthrough). Name match is EXACT
// (no basename) so a path-qualified form cannot be confused with an allowed name (e.g. /tmp/evil/ls is never treated as ls).
func checkAllowlist(command string, allow []string) error {
	if len(allow) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowed[a] = true
	}
	for _, tokens := range tokenize(command) {
		name, ok := commandName(tokens)
		if !ok {
			continue // env-assignment-only statement (VAR=x) or empty — no command to check.
		}
		if !allowed[name] {
			return fmt.Errorf("default mode shell_allowlist: command %q is not allowed", name)
		}
	}
	return nil
}

// checkConfineCD rejects cd/pushd whose target escapes workdir: absolute path, .. that climbs above, ~, $VAR, bare cd
// (→HOME), or cd - (→previous dir). Conservative — a target we cannot resolve statically is rejected.
func checkConfineCD(command, workdir string) error {
	for _, tokens := range tokenize(command) {
		name, ok := commandName(tokens)
		if !ok || (name != "cd" && name != "pushd") {
			continue
		}
		target, hasTarget := cdTarget(tokens)
		if !hasTarget {
			return fmt.Errorf("default mode shell_confine_cd: %s without an in-workdir target is blocked (bare cd goes HOME)", name)
		}
		if target == "-" || strings.HasPrefix(target, "~") || strings.Contains(target, "$") {
			return fmt.Errorf("default mode shell_confine_cd: %s target %q may resolve outside workdir (home/variable/previous)", name, target)
		}
		if targetEscapesWorkdir(workdir, target) {
			return fmt.Errorf("default mode shell_confine_cd: %s target %q escapes workdir", name, target)
		}
	}
	return nil
}

// commandName returns the leading command token after skipping env-assignment prefixes (VAR=val cmd → cmd).
// ok=false when the segment is assignment-only or empty.
func commandName(tokens []string) (string, bool) {
	i := 0
	for i < len(tokens) && isEnvAssign(tokens[i]) {
		i++
	}
	if i >= len(tokens) {
		return "", false
	}
	return tokens[i], true
}

// isEnvAssign reports whether tok is a leading VAR=value assignment (letter/_ then word chars then =).
func isEnvAssign(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range tok[:eq] {
		if r == '=' {
			return false
		}
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0: // digits allowed only after the first char
		default:
			return false
		}
	}
	return true
}

// cdTarget returns the first non-option argument of a cd/pushd command (options start with '-'). hasTarget=false when
// only options remain (e.g. "cd -", "pushd -n") — caller treats bare/option-only cd as blocked.
func cdTarget(tokens []string) (string, bool) {
	i := 0
	for i < len(tokens) && isEnvAssign(tokens[i]) {
		i++
	}
	i++ // skip the cd/pushd command name itself
	for i < len(tokens) {
		t := tokens[i]
		if t == "--" { // end-of-options marker: next token is the target verbatim
			i++
			if i < len(tokens) {
				return tokens[i], true
			}
			return "", false
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			i++
			continue // -P/-L/-e/-n etc. (cd - is handled by the caller as "previous dir")
		}
		return t, true
	}
	return "", false
}

// targetEscapesWorkdir reports whether target (relative to workdir or absolute) falls outside the workdir subtree after
// Clean. Pure lexical (no symlink walk) — static cd-target guard; TOCTOU/symlink escapes are the OS layer's job.
func targetEscapesWorkdir(workdir, target string) bool {
	full := target
	if !filepath.IsAbs(target) {
		full = filepath.Join(workdir, target)
	}
	abs := filepath.Clean(full)
	rootAbs := filepath.Clean(workdir)
	sep := string(filepath.Separator)
	return !strings.HasPrefix(abs+sep, rootAbs+sep)
}

// tokenize splits a shell command string into simple commands, each a slice of word tokens. It is a pragmatic
// best-effort scanner — enough for command-name and cd-target extraction, NOT a full POSIX shell parser.
//
// Handled: single/double quotes and backslash escapes (quoted separators do not split), command separators ; | & \n
// (&& and || split too since each char ends a command), subshell/group braces (){} as in-word separators (so (cd /x)
// is seen as cd), and leading VAR=val assignment prefixes. Redirection chars <> are kept as word characters so common
// fd-redirects like 2>&1 do not split spuriously (the & in >&/<& is detected via the preceding >/<).
//
// NOT handled (documented blind spots — bypassable, consistent with the sudo/su denylist): eval, $()/backtick command
// substitution, here-docs, arithmetic, aliases, and quote-aware keyword constructs (for/while/if/case). These remain
// guardrail gaps, not security holes (invariant #13).
func tokenize(cmd string) [][]string {
	runes := []rune(cmd)
	var commands [][]string
	var tokens []string
	var buf []rune
	inSingle, inDouble := false, false

	flushWord := func() {
		if len(buf) > 0 {
			tokens = append(tokens, string(buf))
			buf = buf[:0]
		}
	}
	flushCmd := func() {
		flushWord()
		if len(tokens) > 0 {
			commands = append(commands, tokens)
			tokens = nil
		}
	}
	endsRedirect := func() bool {
		return len(buf) > 0 && (buf[len(buf)-1] == '>' || buf[len(buf)-1] == '<')
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				buf = append(buf, r)
			}
		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\': // inside double quotes backslash escapes the next char (sufficient for tokenization)
				if i+1 < len(runes) {
					buf = append(buf, runes[i+1])
					i++
				} else {
					buf = append(buf, r)
				}
			default:
				buf = append(buf, r)
			}
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		case r == '\\': // outside quotes backslash escapes the next char
			if i+1 < len(runes) {
				buf = append(buf, runes[i+1])
				i++
			}
		case r == '&' && i+1 < len(runes) && runes[i+1] == '&':
			flushCmd() // && command list
			i++
		case r == '&' && endsRedirect():
			buf = append(buf, r) // >& / <& fd redirect — keep as part of the word, do not split
		case r == '&' || r == ';' || r == '|' || r == '\n':
			flushCmd() // command separator (single & = background; | / || / ; / newline)
		case r == ' ' || r == '\t' || r == '\r':
			flushWord()
		case r == '(' || r == ')' || r == '{' || r == '}':
			flushWord() // subshell/group braces: word separators within a command (not command separators)
		default:
			buf = append(buf, r)
		}
	}
	flushCmd()
	return commands
}
