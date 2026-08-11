package tools

import (
	"context"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"strings"
	"testing"
)

// guardShell mirrors buildTools' composition (ShellTool + optional GuardShell) so the tools-package tests exercise the
// real wrapper without depending on the cmd/miniagent package.
func guardShell(workdir, mode string, allowlist []string, confineCD bool) miniagent.Tool {
	s := ShellTool(workdir, 0, mode, 0, 0)
	if mode == miniagent.ModeDefault && (confineCD || len(allowlist) > 0) {
		s = GuardShell(s, workdir, allowlist, confineCD)
	}
	return s
}

// --- checkAllowlist (unit) ---

func TestCheckAllowlist_AllowedPasses(t *testing.T) {
	if err := checkAllowlist("ls -la", []string{"ls"}); err != nil {
		t.Errorf("allowed command rejected: %v", err)
	}
}

func TestCheckAllowlist_DisallowedRejected(t *testing.T) {
	if err := checkAllowlist("rm -rf x", []string{"ls"}); err == nil {
		t.Error("disallowed command should be rejected")
	}
}

// Every command in a pipeline/list is checked: the second segment (rm) is not in the allowlist.
func TestCheckAllowlist_PipeChecksEveryCommand(t *testing.T) {
	for _, cmd := range []string{
		"echo ok | rm x",
		"echo ok; rm x",
		"echo ok && rm x",
		"echo ok\nrm x",
	} {
		if err := checkAllowlist(cmd, []string{"echo"}); err == nil {
			t.Errorf("pipeline/list %q: rm should be rejected", cmd)
		}
	}
}

// A separator inside quotes does not split: echo "a;b" is a single echo command.
func TestCheckAllowlist_QuotedSeparatorDoesNotSplit(t *testing.T) {
	if err := checkAllowlist(`echo "a;b"`, []string{"echo"}); err != nil {
		t.Errorf("quoted separator should not split: %v", err)
	}
}

// Leading VAR=val assignment prefix is skipped; the real command name is what is checked.
func TestCheckAllowlist_EnvAssignPrefixSkipped(t *testing.T) {
	if err := checkAllowlist("FOO=bar ls", []string{"ls"}); err != nil {
		t.Errorf("env-assign prefix should be skipped: %v", err)
	}
}

// Exact match only: a path-qualified form (/bin/ls) must be listed verbatim, never confused with ls.
func TestCheckAllowlist_PathQualifiedNotMatched(t *testing.T) {
	if err := checkAllowlist("/bin/ls", []string{"ls"}); err == nil {
		t.Error("path-qualified /bin/ls must not match allowlist entry ls (exact match only)")
	}
	if err := checkAllowlist("/usr/bin/git status", []string{"git"}); err == nil {
		t.Error("path-qualified /usr/bin/git must not match allowlist entry git")
	}
}

// Empty allowlist = no check (passthrough), even for a destructive-looking command.
func TestCheckAllowlist_EmptyAllowlistPassthrough(t *testing.T) {
	if err := checkAllowlist("rm -rf /", []string{}); err != nil {
		t.Errorf("empty allowlist should not check: %v", err)
	}
}

// A 2>&1 fd-redirect does not split spuriously (the & in >& is kept in-word), so make is seen as the command name.
func TestCheckAllowlist_FdRedirectDoesNotSplit(t *testing.T) {
	if err := checkAllowlist("make 2>&1", []string{"make"}); err != nil {
		t.Errorf("2>&1 should not split the command: %v", err)
	}
	if err := checkAllowlist("make 2>&1 | grep err", []string{"make", "grep"}); err != nil {
		t.Errorf("piped fd-redirect should check make and grep: %v", err)
	}
}

// Background & separates commands; both are checked.
func TestCheckAllowlist_BackgroundAmpersandSplits(t *testing.T) {
	if err := checkAllowlist("echo a & echo b", []string{"echo"}); err != nil {
		t.Errorf("background & both echo should pass: %v", err)
	}
	if err := checkAllowlist("echo a & rm b", []string{"echo"}); err == nil {
		t.Error("background & : rm should be rejected")
	}
}

// --- checkConfineCD (unit) ---

func TestCheckConfineCD_InWorkdirAllowed(t *testing.T) {
	for _, cmd := range []string{"cd sub", "cd sub/deep", "cd .", "cd sub/../other"} {
		if err := checkConfineCD(cmd, "/work"); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}
}

func TestCheckConfineCD_AbsoluteInsideWorkdirAllowed(t *testing.T) {
	if err := checkConfineCD("cd /work/sub", "/work"); err != nil {
		t.Errorf("absolute in-workdir target should be allowed: %v", err)
	}
	if err := checkConfineCD("cd /work", "/work"); err != nil {
		t.Errorf("cd to the workdir root itself should be allowed: %v", err)
	}
}

func TestCheckConfineCD_EscapesRejected(t *testing.T) {
	for _, cmd := range []string{
		"cd /etc",
		"cd ..",
		"cd ../..",
		"cd ~",
		"cd $HOME",
		"cd -",
		"pushd /tmp",
	} {
		if err := checkConfineCD(cmd, "/work"); err == nil {
			t.Errorf("%q should be rejected (escapes workdir)", cmd)
		}
	}
}

// Bare cd (→HOME) and option-only cd have no in-workdir target → blocked.
func TestCheckConfineCD_BareAndOptionOnlyBlocked(t *testing.T) {
	for _, cmd := range []string{"cd", "cd -P"} {
		if err := checkConfineCD(cmd, "/work"); err == nil {
			t.Errorf("%q should be blocked (no in-workdir target)", cmd)
		}
	}
}

// No cd in the command → passes.
func TestCheckConfineCD_NoCdPasses(t *testing.T) {
	if err := checkConfineCD("ls -la /tmp", "/work"); err != nil {
		t.Errorf("command without cd should pass: %v", err)
	}
}

// cd in a non-first segment (pipe) is still caught.
func TestCheckConfineCD_CdInPipeCaught(t *testing.T) {
	if err := checkConfineCD("echo hi | cd /etc", "/work"); err == nil {
		t.Error("cd /etc in a pipe should be rejected")
	}
}

// Subshell parens are word separators, so (cd /etc) is seen as cd.
func TestCheckConfineCD_SubshellCaught(t *testing.T) {
	if err := checkConfineCD("(cd /etc)", "/work"); err == nil {
		t.Error("(cd /etc) subshell should be rejected")
	}
}

// cd -P /etc does not bypass via an option flag.
func TestCheckConfineCD_OptionFlagDoesNotBypass(t *testing.T) {
	if err := checkConfineCD("cd -P /etc", "/work"); err == nil {
		t.Error("cd -P /etc should be rejected")
	}
}

// --- targetEscapesWorkdir (unit) ---

func TestTargetEscapesWorkdir(t *testing.T) {
	for _, c := range []struct {
		target string
		want   bool
	}{
		{"sub", false},
		{"sub/deep", false},
		{".", false},
		{"/work/sub", false},
		{"/work", false},
		{"..", true},
		{"../..", true},
		{"/etc", true},
		{"/etc/passwd", true},
	} {
		if got := targetEscapesWorkdir("/work", c.target); got != c.want {
			t.Errorf("targetEscapesWorkdir(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

// --- GuardShell integration (real ShellTool + wrapper) ---

// default + allowlist [echo]: an allowed command runs and produces output.
func TestGuardShell_DefaultAllowsListedRuns(t *testing.T) {
	s := guardShell(t.TempDir(), miniagent.ModeDefault, []string{"echo"}, false)
	res := s.Call(context.Background(), `{"command":"echo hi"}`)
	if res.IsError {
		t.Fatalf("allowed echo should run: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Errorf("echo output missing 'hi': %q", res.Output)
	}
}

// default + allowlist [echo]: an unlisted command is pre-rejected (never executed).
func TestGuardShell_DefaultRejectsUnlisted(t *testing.T) {
	s := guardShell(t.TempDir(), miniagent.ModeDefault, []string{"echo"}, false)
	res := s.Call(context.Background(), `{"command":"ls"}`)
	if !res.IsError || !strings.Contains(res.Output, "shell_allowlist") {
		t.Errorf("unlisted ls should be pre-rejected by allowlist: %+v", res)
	}
	if res.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("guard rejection ExitCode = %d, want ExitCodeNotSet", res.ExitCode)
	}
}

// auto mode: the guard is NOT applied, so an unlisted command runs (allowlist ignored) — auto stays unrestricted.
func TestGuardShell_AutoIgnoresAllowlist(t *testing.T) {
	s := guardShell(t.TempDir(), miniagent.ModeAuto, []string{"echo"}, false)
	res := s.Call(context.Background(), `{"command":"ls"}`)
	if strings.Contains(res.Output, "shell_allowlist") {
		t.Errorf("auto mode should not apply allowlist: %s", res.Output)
	}
	if res.IsError {
		t.Errorf("auto mode ls should run: %s", res.Output)
	}
}

// default + confineCD: cd out of workdir is pre-rejected.
func TestGuardShell_DefaultConfineCdRejects(t *testing.T) {
	s := guardShell(t.TempDir(), miniagent.ModeDefault, nil, true)
	res := s.Call(context.Background(), `{"command":"cd /"}`)
	if !res.IsError || !strings.Contains(res.Output, "shell_confine_cd") {
		t.Errorf("cd / should be rejected by confine_cd: %+v", res)
	}
}

// default + confineCD: cd to the current/workdir-relative target is allowed to run.
func TestGuardShell_DefaultConfineCdAllowsInWorkdir(t *testing.T) {
	s := guardShell(t.TempDir(), miniagent.ModeDefault, nil, true)
	res := s.Call(context.Background(), `{"command":"cd ."}`)
	if strings.Contains(res.Output, "shell_confine_cd") {
		t.Errorf("cd . should not be rejected by confine_cd: %s", res.Output)
	}
}

// Empty/missing command passes through to orig (which rejects empty itself) — no spurious guard error.
func TestGuardShell_EmptyCommandFallsThrough(t *testing.T) {
	s := guardShell(t.TempDir(), miniagent.ModeDefault, []string{"echo"}, true)
	for _, args := range []string{`{"command":""}`, `not-json`, `{"command":"   "}`} {
		res := s.Call(context.Background(), args)
		if !res.IsError {
			t.Errorf("args %q: want orig to reject (IsError), got success", args)
		}
		if strings.Contains(res.Output, "shell_allowlist") || strings.Contains(res.Output, "shell_confine_cd") {
			t.Errorf("args %q: guard should pass through, not emit its own rejection: %s", args, res.Output)
		}
	}
}
