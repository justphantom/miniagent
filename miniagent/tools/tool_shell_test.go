package tools

import (
	"context"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShell_RunsCommand(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestShell_CwdIsWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	s := ShellTool(dir, 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"pwd"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	cleaned := filepath.Clean(dir)
	if !strings.Contains(res.Output, cleaned) {
		t.Errorf("Output = %q, want contains %q", res.Output, cleaned)
	}
}

// Non-zero exit is a legitimate command result: IsError=false, ExitCode=the command's exit code, stdout fully retained.
// The old version treated non-zero exit as IsError=true and concatenated the exit code into Output text; it now uses a structured ExitCode.
func TestShell_NonZeroExitReturnsExitCode(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo out; exit 3"}`)
	if res.IsError {
		t.Fatalf("non-zero exit should not be IsError: %s", res.Output)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Output, "out") {
		t.Errorf("stdout lost: Output = %q", res.Output)
	}
}

// Cleanup the whole group after timeout: sh's grandchild processes should not remain.
// Skipped in short mode: the test needs shellTimeout(60s) to elapse, taking too long.
func TestShell_KillsGrandchildOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires shellTimeout to elapse")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available (exec -a needs bash)")
	}
	marker := "miniagent_uniq_sleep_marker_9f3k2"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "pkill", "-9", "-f", marker).Run()
	})
	s := ShellTool(t.TempDir(), 2*time.Second, 0, 0)
	// bash -c 'exec -a marker sleep 600': makes the sleep process name include the marker for pgrep -f to match exactly.
	// Using a 2s timeout (not the default 60s) for speed; still verifies the process group is killed (grandchild cleanup).
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"bash -c 'exec -a `+marker+` sleep 600'"}`)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Error("expected timeout error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
	time.Sleep(time.Second)
	pgrepCtx, pgrepCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pgrepCancel()
	out, err := exec.CommandContext(pgrepCtx, "pgrep", "-f", marker).Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("grandchild still alive after kill: %s", out)
	}
}

// Empty workdir: cmd.Dir is left empty, exec inherits the parent process's cwd.
func TestShell_EmptyWorkdirInheritsCwd(t *testing.T) {
	s := ShellTool("", 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo ok-empty"}`)
	if res.IsError {
		t.Fatalf("empty workdir should not fail: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok-empty") {
		t.Errorf("Output = %q", res.Output)
	}
}

// The child process inherits the parent process's full environment (except the MINIAGENT_* prefix); other variables are passed through as-is.
func TestShell_InheritsFullEnv(t *testing.T) {
	t.Setenv("MINIAGENT_TEST_INHERIT", "inherited")
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo $MINIAGENT_TEST_INHERIT"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "inherited") {
		t.Errorf("MINIAGENT_* should be scrubbed: %q", res.Output)
	}
	// Non-MINIAGENT_ prefixed variables should be inherited normally.
	t.Setenv("TEST_SHELL_INHERIT", "passed-through")
	res2 := s.Call(context.Background(), `{"command":"echo $TEST_SHELL_INHERIT"}`)
	if res2.IsError {
		t.Fatalf("shell failed: %s", res2.Output)
	}
	if !strings.Contains(res2.Output, "passed-through") {
		t.Errorf("non-MINIAGENT env not inherited: %q", res2.Output)
	}
}

// MINIAGENT_API_KEY must be scrubbed, preventing the LLM from reading the host key via shell.
func TestShell_ScrubsAPIKey(t *testing.T) {
	t.Setenv("MINIAGENT_API_KEY", "sk-secret-leak")
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo [$MINIAGENT_API_KEY]"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "sk-secret-leak") {
		t.Errorf("API key leaked to child: %q", res.Output)
	}
}

// All MINIAGENT_* prefixed variables should be scrubbed (including config info such as BASE_URL).
func TestShell_ScrubsAllMiniagentVars(t *testing.T) {
	t.Setenv("MINIAGENT_API_KEY", "sk-leak")
	t.Setenv("MINIAGENT_BASE_URL", "https://private.example.internal")
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"env | grep MINIAGENT_ | wc -l"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if !strings.Contains(strings.TrimSpace(res.Output), "0") {
		t.Errorf("MINIAGENT_* vars leaked: %q", res.Output)
	}
}

// Empty command: argument validation fails.
func TestShell_EmptyCommandRejected(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"   "}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// Custom timeout: sleep 5 is killed after 200ms, returns IsError and contains "timeout"; returns within 1s.
func TestShell_CustomTimeout(t *testing.T) {
	s := ShellTool(t.TempDir(), 200*time.Millisecond, 0, 0)
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"sleep 5"}`)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Fatal("expected timeout error")
	}
	if res.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("ExitCode = %d, want %d (timeout)", res.ExitCode, miniagent.ExitCodeNotSet)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Errorf("Output = %q", res.Output)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
}

// timeout=0 uses the default path (does not actually wait 60s, only verifies normal execution).
func TestShell_ZeroTimeoutUsesDefault(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo ok"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Errorf("Output = %q", res.Output)
	}
}

// shell is registered in auto mode only (default mode does not register it at all, buildTools-level decision);
// ShellTool itself no longer filters privilege escalators — the registration gate replaces the denylist.
// Text mentioning escalators must run verbatim (the denylist removal regression).
func TestShellTool_NoPrivilegeEscalatorPrefilter(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo has sudo in text"}`)
	if res.IsError {
		t.Errorf("shell should not pre-filter escalator-looking text: %s", res.Output)
	}
	if !strings.Contains(res.Output, "sudo") {
		t.Errorf("Output = %q, want the echoed text verbatim", res.Output)
	}
}

// Parent ctx timeout (max-duration / signal) should not be misreported as a shell's own timeout (fix R5):
// set the shell timeout long (does not trigger), the parent ctx short timeout causes sleep to be cancelled -> the output must not contain the "timed out" text.
func TestShell_ParentCancelNotReportedAsShellTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires parent ctx to elapse")
	}
	parent, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	s := ShellTool(t.TempDir(), 60*time.Second, 0, 0)
	res := s.Call(parent, `{"command":"sleep 3"}`)
	if strings.Contains(res.Output, "timed out") {
		t.Errorf("parent cancel misreported as shell timeout: %s", res.Output)
	}
}

// P1-D: shell output exceeding 100k chars keeps the tail (including the end where the exit code is), adds a banner, and the ExitCode is trustworthy (the command ran to completion).
func TestShell_HighOutputKeepsTail(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"seq 1 400000"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (the command should run to completion, exit code is trustworthy)", res.ExitCode)
	}
	if !strings.Contains(res.Output, "only tail kept") {
		t.Errorf("oversized output should contain banner: len=%d", len(res.Output))
	}
	if !strings.Contains(res.Output, "399999") || !strings.Contains(res.Output, "400000") {
		t.Errorf("should keep the tail 399999/400000: len=%d", len(res.Output))
	}
	if strings.Contains(res.Output, "1\n2\n3\n") {
		t.Errorf("should not keep the head 1\\n2\\n3\\n (the middle segment should be dropped)")
	}
}

// P1-D regression: commands producing <100k chars are byte-level equivalent (no banner, head and tail intact).
func TestShell_SmallOutputNoBanner(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"seq 1 100"}`)
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if strings.Contains(res.Output, "only tail kept") {
		t.Errorf("small output should not have a banner")
	}
	if !strings.Contains(res.Output, "1\n") || !strings.Contains(res.Output, "\n100") {
		t.Errorf("small output should fully contain head and tail: %q", res.Output)
	}
}

// P1-D: after removing volume-kill, the ctx timeout semantics still hold (sleep > timeout -> IsError + miniagent.ExitCodeNotSet + timeout hint).
func TestShell_TimeoutStillReported(t *testing.T) {
	s := ShellTool(t.TempDir(), 100*time.Millisecond, 0, 0)
	res := s.Call(context.Background(), `{"command":"sleep 5"}`)
	if !res.IsError {
		t.Errorf("timeout should be IsError=true")
	}
	if res.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("timeout ExitCode = %d, want miniagent.ExitCodeNotSet", res.ExitCode)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Errorf("timeout should contain a hint: %q", res.Output)
	}
}
