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
var deniedGitArgPrefixes = []string{"--output", "-O", "--ext-diff"}

func GitTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	return miniagent.Tool{
		Name:        "git",
		Description: "Git operations: read-only (status/diff/log/show/ls-tree etc) plus basic versioning (add/commit/pull/push). History-rewriting commands (reset/rebase/merge/checkout) and config/branch/tag management are blocked.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Git subcommand"},
			"args":       map[string]any{"type": "string", "description": "Additional arguments (whitespace-split; options that write files are rejected)"},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
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
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v", err)}
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
	fields := strings.Fields(a.Args)
	for _, f := range fields {
		for _, p := range deniedGitArgPrefixes {
			if strings.HasPrefix(f, p) {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("git %s option %q writes a file or runs an external program; blocked", a.Subcommand, f)}
			}
		}
	}
	dir, err := resolveGitRoot(workspaceRoot)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("not a git repository: %v", err)}
	}
	cmdArgs := []string{"-C", dir, "--no-pager", a.Subcommand}
	cmdArgs = append(cmdArgs, fields...)
	bin, argv := "git", cmdArgs
	if rtkGitSubcommands[a.Subcommand] {
		bin, argv = rtkWrap("git", []string{"git", "-C", dir, "--no-pager", a.Subcommand}, fields)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = dir
	cmd.Env = scrubEnv(os.Environ())
	body, err := runLimitedOutput(ctx, cmd, maxShellOutputChars)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("git %s failed: %v\n%s", a.Subcommand, err, body)}
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
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

func runLimitedOutput(ctx context.Context, cmd *exec.Cmd, maxOutputChars int) (string, error) {
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
	accum := newOutputAccum(maxOutputChars, 0, "", "miniagent_git_")
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
	return accum.finalize(maxOutputChars), werr
}
