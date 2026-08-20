package policy

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/justphantom/miniagent/miniagent"
)

// DefaultDangerWords are substrings in the "shell" tool's raw arguments that trigger the confirmation gate (in addition
// to write/edit, which are always gated). A conservative heuristic, not a command parser — it raises the bar for the
// common irreversible cases (force-remove, privilege escalation, disk/partition wipe, system power); it is deliberately
// bypassable, since a confirmation layer is not a sandbox (see docs S-3).
var DefaultDangerWords = []string{
	"rm -rf", "rm -fr", "sudo", "mkfs", "reboot", "shutdown", "dd if=", "> /dev/sd", ":(){", "chmod -R",
}

// ConfirmCfg controls the destructive-tool confirmation gate.
type ConfirmCfg struct {
	// Enabled is the master switch. false (default) → ConfirmOnToolUse returns emit unchanged (identity, zero overhead,
	// current behavior). true → write/edit are always gated; shell is gated on DangerWords.
	Enabled bool
	// AutoApprove bypasses the interactive prompt for CI / trusted autonomous runs (set via MINIAGENT_AUTO_APPROVE=1).
	AutoApprove bool
	// Stdin/Stdout override the prompt I/O (nil → os.Stdin / os.Stderr). The prompt goes to Stderr, not Stdout, because
	// Stdout is the NDJSON event stream and must stay machine-readable.
	Stdin  io.Reader
	Stdout io.Writer
	// DangerWords overrides DefaultDangerWords for shell gating. nil → DefaultDangerWords.
	DangerWords []string
}

// destructiveTools are tool names always gated (overwrite/delete semantics).
var destructiveTools = map[string]bool{"write": true, "edit": true}

// ConfirmOnToolUse wraps an existing OnToolUse emit callback with a destructive-tool confirmation gate.
//
// Design (docs/long-session-comprehensive-assessment.md §7 P0-2):
//   - ORDER: emit runs FIRST, then the gate — so an upstream pipe-closed error still terminates the loop
//     (handleToolCalls treats a non-ErrToolDenied OnToolUse error as terminal). Reversing the order would swallow that
//     path and let a destructive op proceed after the consumer is gone.
//   - DENY-BY-DEFAULT: when the gate cannot prompt (no TTY — subagent fork / -result-only / CI) and AutoApprove is
//     unset, destructive tools are DENIED. This is the safe failure mode and is what covers the subagent path
//     (buildHooks returns empty hooks for -result-only; wiring the gate after buildHooks makes it active there too).
//   - Denials return a miniagent.ErrToolDenied wrapper so handleToolCalls skips just that tool without terminating the
//     loop (the core skip+backfill semantics, already built).
//   - Disabled → identity, so opting out leaves behavior (and the subagent path) untouched.
func ConfirmOnToolUse(emit miniagent.OnToolUse, cfg ConfirmCfg) miniagent.OnToolUse {
	if !cfg.Enabled {
		return emit
	}
	stdin := cfg.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stderr
	}
	words := cfg.DangerWords
	if words == nil {
		words = DefaultDangerWords
	}
	return func(name, input string) error {
		if emit != nil {
			if err := emit(name, input); err != nil {
				return err // upstream terminate (e.g. pipe closed): propagate before the gate.
			}
		}
		if !needsConfirm(name, input, words) {
			return nil
		}
		if cfg.AutoApprove {
			return nil
		}
		if !isTerminal(stdin) {
			return fmt.Errorf("%w: destructive tool %q in non-interactive context (set MINIAGENT_AUTO_APPROVE=1 to allow)", miniagent.ErrToolDenied, name)
		}
		if !readYes(stdin, stdout, name, input) {
			return fmt.Errorf("%w: destructive tool %q denied by user", miniagent.ErrToolDenied, name)
		}
		return nil
	}
}

// needsConfirm reports whether the tool invocation should be gated.
func needsConfirm(name, input string, dangerWords []string) bool {
	if destructiveTools[name] {
		return true
	}
	if name == "shell" {
		for _, w := range dangerWords {
			if w != "" && strings.Contains(input, w) {
				return true
			}
		}
	}
	return false
}

// isTerminal reports whether r is an interactive terminal (a char device). Non-*os.File readers (test stubs, pipes,
// redirected stdin) report false — the safe non-interactive answer.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes prompts y/N on stdout and reads one line from stdin; true only on an explicit y/yes.
func readYes(stdin io.Reader, stdout io.Writer, name, input string) bool {
	fmt.Fprintf(stdout, "⚠  miniagent: destructive tool %q\n%s\nProceed? [y/N]: ", name, truncateForPrompt(input))
	sc := bufio.NewScanner(stdin)
	if !sc.Scan() {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(sc.Text()))
	return s == "y" || s == "yes"
}

func truncateForPrompt(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
