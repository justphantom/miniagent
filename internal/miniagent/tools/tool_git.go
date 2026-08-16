package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type gitArgs struct {
	Subcommand string `json:"subcommand"`
	Args       string `json:"args,omitempty"`
}

// 只读子命令 + 基本版本写操作（add/commit/pull/push）。
// 写操作经 rtk 代理时输出紧凑（"ok <hash>"/"ok <branch>"）；reset/merge/rebase/checkout 等改变历史的命令仍被拒。
var allowedGitSubcommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true,
	"ls-files": true, "blame": true, "reflog": true,
	"whatchanged": true, "describe": true, "check-attr": true,
	"ls-tree": true, "rev-parse": true, "shortlog": true, "cat-file": true,
	"add": true, "commit": true, "pull": true, "push": true,
}

// rtkGitSubcommands lists the subcommands that rtk git supports (compact output).
// Only these route through rtk; the rest exec native git for raw output.
var rtkGitSubcommands = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "add": true, "commit": true, "pull": true, "push": true}

// deniedGitArgPrefixes: --output/-O write report files anywhere; --ext-diff runs an arbitrary diff driver.
// --no-index makes diff compare arbitrary files OUTSIDE the repo (out-of-tree read); -F reads the commit
// message from an arbitrary file whose content then surfaces via log/show (out-of-tree read channel).
// --amend rewrites the last commit (history rewriting, matching the blocked reset/rebase class);
// --force/--force-with-lease let push overwrite remote refs (remote history rewrite); --delete lets push
// remove a remote ref. Descriptions promise "history-rewriting blocked" — these close the in-subcommand gap.
var deniedGitArgPrefixes = []string{"--output", "-O", "--ext-diff", "--no-index", "-F", "--amend", "--force", "--force-with-lease", "--delete"}

func GitTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	return miniagent.Tool{
		Name:        "git",
		Description: "Git operations: read-only (status/diff/log/show/ls-tree etc) plus basic versioning (add/commit/pull/push). History-rewriting commands (reset/rebase/merge/checkout, commit --amend, push --force/--delete) and config/branch/tag management are blocked. Scope is the WHOLE repository (upward .git discovery), not just the workdir — pass explicit pathspecs (e.g. add -- <subdir>) to limit range. When the rtk proxy is deployed, output is compact and NOT native git format. Commit requires -m.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Git subcommand"},
			"args":       map[string]any{"type": "string", "description": `Additional arguments as ONE string; shell-style quoting keeps spaces intact (e.g. args: "-m \"feat: add thing\""). Options that write files are rejected`},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		// 错误结论（conflict/failed hunk/summary）在输出尾部，head 截断会丢失；与 shell/grep 同取 head+tail。
		SplitTruncate: true,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "git", func(rctx context.Context) miniagent.ToolResult {
				return runGit(rctx, workspaceRoot, args)
			})
		},
	}
}

func runGit(ctx context.Context, workspaceRoot, args string) miniagent.ToolResult {
	var a gitArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed (args must be a JSON object with string fields, e.g. {\"subcommand\":\"status\",\"args\":\"-m \\\"msg\\\"\"}): %v", err)}
	}
	if strings.TrimSpace(a.Subcommand) == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedGitSubcommands[a.Subcommand] {
		return miniagent.ToolResult{
			IsError: true,
			Output:  fmt.Sprintf("git %q is not in the allow-list; permitted: status, diff, log, show, ls-files, blame, reflog, whatchanged, describe, check-attr, ls-tree, rev-parse, shortlog, cat-file, add, commit, pull, push", a.Subcommand),
		}
	}
	fields := splitArgs(a.Args)
	for _, f := range fields {
		for _, p := range deniedGitArgPrefixes {
			if strings.HasPrefix(f, p) {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("git %s option %q writes a file or runs an external program; blocked", a.Subcommand, f)}
			}
		}
	}
	if err := checkGitPositionalArgs(a.Subcommand, fields); err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	// commit without -m would open the configured editor (blocked non-interactively above → empty-message abort);
	// rejecting up front gives the LLM an actionable message instead of a confusing editor failure.
	if a.Subcommand == "commit" && !hasGitMessageFlag(fields) {
		return miniagent.ToolResult{IsError: true, Output: `git commit requires -m "message" in args`}
	}
	dir, err := resolveGitRoot(workspaceRoot)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("not a git repository: %v", err)}
	}
	if err := checkGitAttributes(dir); err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	cmdArgs := []string{"-C", dir, "--no-pager", a.Subcommand}
	cmdArgs = append(cmdArgs, fields...)
	bin, argv := "git", cmdArgs
	if rtkGitSubcommands[a.Subcommand] {
		bin, argv = rtkWrap("git", []string{"git", "-C", dir, "--no-pager", a.Subcommand}, fields)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = dir
	cmd.Env = append(scrubEnv(os.Environ()),
		// Non-interactive hardening: a repo with core.editor set (or EDITOR inherited) makes commit/tag
		// open an editor that blocks until the 120s timeout with no useful output. GIT_EDITOR=true makes
		// git accept the message as-is; GIT_TERMINAL_PROMPT=0 turns credential prompts into an immediate
		// error instead of a TTY hang; empty stdin makes a hook/prompt waiting on input see EOF.
		"GIT_EDITOR=true", "GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0",
	)
	if a.Subcommand == "push" || a.Subcommand == "pull" {
		// push/pull need remote auth (GITHUB_TOKEN/SSH_AUTH_SOCK were scrubbed as secret-named);
		// their output never echoes env, so restoring just these keeps the allow-listed workflow
		// functional — every other subcommand (incl. commit, which runs hooks) stays fully scrubbed.
		cmd.Env = restoreGitCredentials(cmd.Env, os.Environ())
	}
	cmd.Stdin = nil
	body, err := runLimitedOutput(ctx, cmd)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("git %s failed: %v\n%s", a.Subcommand, err, body)}
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}

// hasGitMessageFlag reports whether args carry a commit message flag (-m or -F's short form is
// deny-listed, so only -m/--message= apply). Bare `git commit` would open an editor.
func hasGitMessageFlag(fields []string) bool {
	for _, f := range fields {
		if f == "-m" || f == "--message" || strings.HasPrefix(f, "--message=") {
			return true
		}
	}
	return false
}

// checkGitPositionalArgs rejects a repository URL where a refspec is expected. `git push <url> ...` /
// `git pull <url> ...` would target a remote other than the configured one — the exfiltration channel that
// remains after .git/config writes were blocked (the deny-prefix list cannot see positional args).
// A refspec never contains "://"; absolute local paths are rejected the same way (push to a local repo copy).
func checkGitPositionalArgs(subcommand string, fields []string) error {
	if subcommand != "push" && subcommand != "pull" {
		return nil
	}
	for _, f := range fields {
		if strings.HasPrefix(f, "-") {
			continue
		}
		if strings.Contains(f, "://") || filepath.IsAbs(f) {
			return fmt.Errorf("git %s: %q looks like a repository URL/path, not a refspec; push/pull operate on the configured remote only (default mode)", subcommand, f)
		}
		break // first non-option positional is the only URL slot; the rest are refspecs
	}
	return nil
}

// checkGitAttributes rejects git operations whose clean/smudge filters or diff drivers would execute an
// external program: a workdir-writable .gitattributes declaring `filter=<name>` / `diff=<driver>` /
// `textconv=<cmd>` attributes turns `git add`/`git diff` into arbitrary-command execution — no .git access
// needed, so the .git lock does not cover it. Checks the repo-root .gitattributes (repo-wide scope;
// per-directory files affect only their subtree and the common case is the root file). The attribute VALUE
// is what names the driver, so any `filter=`/`diff=`/`textconv=` token is rejected conservatively —
// built-in `diff` drivers are rare in attributes and a false positive costs one manual edit, while a missed
// one costs code execution. Guardrail, not a boundary — incoming attributes via pull are supply-chain
// exposure by definition and are not pre-checkable.
func checkGitAttributes(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	if err != nil {
		return nil // absent/unreadable: no extra drivers declared
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for tok := range strings.FieldsSeq(line) {
			if strings.HasPrefix(tok, "filter=") || strings.HasPrefix(tok, "diff=") || strings.HasPrefix(tok, "textconv=") {
				return fmt.Errorf(".gitattributes declares external driver %q (filter/diff/textconv execute commands; default mode) — remove the line or use -mode auto", tok)
			}
		}
	}
	return nil
}

func resolveGitRoot(startDir string) (string, error) {
	dir := startDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("not a git repository")
}

func runLimitedOutput(ctx context.Context, cmd *exec.Cmd) (string, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", err
	}
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		_ = pw.Close()
	}()
	go func() {
		<-ctx.Done()
		killProcessGroup(cmd)
		_ = pw.Close()
	}()
	accum := newOutputAccum(maxShellOutputChars, 0, "", "miniagent_git_")
	buf := make([]byte, 32*1024)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			_ = accum.write(string(buf[:n]))
		}
		if rerr != nil {
			break
		}
	}
	_ = accum.closeSink()
	werr := <-waitErr
	killProcessGroup(cmd)
	return accum.finalize(maxShellOutputChars), werr
}
