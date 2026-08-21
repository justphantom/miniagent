package tools

import (
	"context"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxShellOutputChars is the shared tool output character cap for shell/glob/grep: 100KB covers typical command output.
// Overridden at runtime via miniagent.Limits.MaxShellOutputChars (<=0 uses this default). streamWindow defaults to maxOutputChars*8.
const maxShellOutputChars = 100000

const shellTimeout = 120 * time.Second

// ShellTool returns a shell tool bound to workspaceRoot. timeout<=0 uses the default shellTimeout.
// When workspaceRoot is empty cmd.Dir is left empty and exec inherits the parent process's cwd.
// The caller decides whether to register it at all (cmd/miniagent buildTools: auto-only — default
// mode does not register shell), so there is no mode parameter here.
func ShellTool(workspaceRoot string, timeout time.Duration, maxOutputChars, streamWindow int) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	if streamWindow <= 0 {
		streamWindow = maxOutputChars * 8
	}
	return miniagent.Tool{
		Name:        "shell",
		Description: "Runs a shell command via sh -c. Returns merged stdout+stderr output. The command runs at most " + timeout.String() + "; output exceeding " + strconv.Itoa(maxOutputChars) + " characters is truncated.",
		Parameters: object(map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to execute"},
		}, "command"),
		ResultLimit:   miniagent.MaxToolResultInHistory,
		SplitTruncate: true, // the error conclusion of shell output (exit status / FAIL) is often at the tail; front-truncation would lose it
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			var a struct {
				Command string `json:"command"`
			}
			if err := decodeStrict(args, &a); err != nil {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed (strict decoding, unknown fields rejected — args must be a JSON object with the single string field command): %v (received %q)", err, args)}
			}
			if strings.TrimSpace(a.Command) == "" {
				return miniagent.ToolResult{IsError: true, Output: "missing argument: command"}
			}
			return runShellCommand(ctx, workspaceRoot, a.Command, timeout, maxOutputChars, streamWindow)
		},
	}
}

// runShellCommand executes command (via sh -c), including env scrubbing / process group / timeout / output truncation / exit-code mapping.
// timeout<=0 uses the default shellTimeout. Distinguishes the shell's own timeout from parent ctx cancellation: only when the parent ctx is not cancelled
// and runCtx expires is it the shell's own timeout; a non-zero exit is a legitimate command result (IsError=false, the LLM decides success via ExitCode).
func runShellCommand(ctx context.Context, workdir, command string, timeout time.Duration, maxOutputChars, streamWindow int) miniagent.ToolResult {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	if streamWindow <= 0 {
		streamWindow = maxOutputChars * 8
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = workdir
	// Scrub MINIAGENT_* prefixed entries and entries whose names contain secret-related keywords, lowering the chance
	// the LLM directly echoes host config/credentials; this is not an isolation boundary, see scrubEnv comment.
	cmd.Env = scrubEnv(os.Environ())
	// Separate process group: on timeout kill(-pgid) can also clean up the grandchild processes spawned by sh,
	// otherwise make/find and the like would go orphan and keep running.
	setPGID(cmd)
	body, err := runShellLimited(runCtx, cmd, maxOutputChars, streamWindow)
	if err != nil {
		// runCtx is a child of ctx; a parent timeout also expires runCtx; only when the parent ctx is not cancelled is it the shell's own timeout.
		if ctx.Err() == nil && runCtx.Err() != nil {
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\n⏱ command timed out (>%s), terminated.", timeout)}
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() < 0 {
				// Killed by signal (including the SIGKILL from parent ctx cancellation and the process-group cleanup after a shell timeout): not a legitimate command exit,
				// recorded as IsError + miniagent.ExitCodeNotSet — upholding the "miniagent.ExitCodeNotSet ⟺ IsError" convention (consistent with timeout/cancellation),
				// so the LLM never sees the contradictory IsError=false + negative exit code.
				return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\ncommand terminated by signal: %v.", ee)}
			}
			// The exit code is ALSO appended to Output text: IsError/ExitCode never reach the wire
			// (L0 #8), so a silent failure (empty output, exit 1) would read as success. ExitCode
			// itself is kept for the NDJSON event layer's structured field.
			return miniagent.ToolResult{Output: body + fmt.Sprintf("\n[exit %d]", ee.ExitCode()), ExitCode: ee.ExitCode()}
		}
		return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\nexecution failed: %v", err)}
	}
	// Background & jobs can hold the inherited stdout past the shell leader's clean exit: cmd.Wait
	// blocks until timeout, the group is SIGKILLed, and Wait then reports the leader's OLD status nil —
	// without this branch a 1ms command silently consumes the whole timeout as a clean success.
	if ctx.Err() == nil && runCtx.Err() != nil {
		return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\n⏱ command timed out (>%s), background output holders terminated.", timeout)}
	}
	return miniagent.ToolResult{Output: body, ExitCode: 0}
}

func runShellLimited(ctx context.Context, cmd *exec.Cmd, maxOutputChars, streamWindow int) (string, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", err
	}
	// exec does not actively close the io.PipeWriter; after ctx times out the main process has been killed by
	// CommandContext, but pw is still open and the read loop would block forever. Here we listen on ctx, and once
	// done we close pw and kill the whole group so the read loop unblocks and cmd.Wait returns a kill-error.
	go func() {
		<-ctx.Done()
		killProcessGroup(cmd)
		_ = pw.Close()
	}()
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		_ = pw.Close()
	}()
	// §P1-D: byte sliding-window accumulator — keeps the most recent keep bytes (tail) during execution, drops the middle when over the window (preserving the tail where shell errors/exit codes live);
	// the subprocess never blocks because the pipe is continuously drained, and is not interrupted by output volume (removing the old LimitReader+volume-kill), running until Wait returns a trustworthy ExitCode.
	// phase-1 disk spill is off by default (headSpillBytes=0).
	accum := newOutputAccum(streamWindow, 0, "", "miniagent_shell_")
	// drainPipe 的跨块 UTF-8 pending 缓冲同样适用于 shell：逐块 string(buf[:n]) 会把跨 Read 边界的
	// rune 拆成两段无效字节，窗口逐出后尾部以半个 rune 开头（与 git/go/npm/lint 同一缺陷类）。
	drainPipe(pr, accum)
	_ = accum.closeSink()
	// 有界等待的基线取 ctx 剩余期限；waitOutputTimeout 处理 setsid 孤孙子进程持管道导致的挂死。
	budget := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		budget = max(time.Until(dl), 0)
	}
	err := waitOutputTimeout(waitErr, budget)
	// Fallback: clean up the whole group once more after normal exit, to prevent leftover background & jobs (no longer for volume kill).
	killProcessGroup(cmd)
	return accum.finalize(maxOutputChars), err
}

// scrubEnv copies env and removes: all MINIAGENT_* prefixed entries, and entries whose variable name (uppercased) contains
// KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/PASSPHRASE/AUTH/PAT. The latter covers the source variable injected in
// config mode via ${MAIN_API_KEY} (not MINIAGENT_ prefixed but equally carrying a real key), as well as high-frequency host
// credentials such as AWS_ACCESS_KEY_ID, GH_TOKEN/GITHUB_TOKEN/GITHUB_PAT/GITLAB_PAT, DATABASE_PASSWORD,
// MYSQL_PWD, DB_PASS/REDIS_PASS, GPG_PASSPHRASE, BASIC_AUTH/AUTH_HEADER, lowering the chance the LLM echoes a key out via
// an environment variable. Short keywords like PWD/PASS/AUTH widen the false-positive surface (e.g. AUTHPROXY, PASSWORDLESS
// contain PASS/PASSWORD) — a known trade-off tilted toward security, preferring over-scrubbing over leakage. PAT specifically
// excludes the PATH family (PATH/PATHEXT/*_PATH), see hasSecretKeyword comment.
//
// Known gaps (relying on the caller's OS isolation as a fallback; not force-scrubbed to avoid breaking the environment the
// agent's own shell commands need): DATABASE_URL/SERVICE_URL (URL too broad), *_COOKIE, *_DSN, *_CONN.
// These narrow the incremental leakage surface, they are not a key isolation boundary — unlisted credential names are still
// inherited, and a subprocess can read the full pre-exec environment snapshot via /proc/$PPID/environ. The thorough solution
// is caller-side isolation (container/dedicated UID); if a key is injected via $MINIAGENT_API_KEY it is necessarily in the
// process env and readable via procfs.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "MINIAGENT_") {
			continue
		}
		name := kv
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if hasSecretKeyword(strings.ToUpper(name)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasSecretKeyword reports whether the uppercased variable name contains a secret-related keyword. Serves scrubEnv only.
// hasSecretKeyword reports whether the uppercased variable name contains a secret-related keyword.
// PAT covers fine-grained tokens like GITHUB_PAT, but the PATH family of variables shares the P-A-T substring — PATH is the
// required variable for shell executable-path resolution, and scrubbing it would break ls/grep/cat entirely. So PAT goes
// through a separate "if it contains PATH it must be path-like, exempt" branch. Rare variables like COMPAT_*/PATCH_* are
// over-scrubbed, accepted.
func hasSecretKeyword(upperName string) bool {
	for _, kw := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PWD", "PASS", "PASSPHRASE", "AUTH"} {
		if strings.Contains(upperName, kw) {
			return true
		}
	}
	return strings.Contains(upperName, "PAT") && !strings.Contains(upperName, "PATH")
}
