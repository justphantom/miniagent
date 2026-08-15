package tools

import (
	"context"
	"encoding/json"
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

var allowedGitSubcommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true,
	"tag": true, "remote": true, "ls-files": true, "blame": true, "grep": true,
	"worktree": true, "stash": true, "reflog": true, "config": true,
	"whatchanged": true, "describe": true, "check-attr": true, "ls-tree": true,
	"rev-parse": true, "shortlog": true, "cat-file": true,
}

func GitTool(workspaceRoot string, timeout time.Duration, confineSymlinks bool) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return miniagent.Tool{
		Name:        "git",
		Description: "Read-only git operations. Blocks destructive commands like push/pull/commit/reset.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Git subcommand"},
			"args":       map[string]any{"type": "string", "description": "Additional arguments"},
		}, "subcommand"),
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
			Output: fmt.Sprintf("git %q is not in the allow-list; blocked as potentially destructive. Use allowed read-only subcommands: status, diff, log, show, branch, tag, remote, ls-files, blame, grep, worktree, stash, reflog, config, whatchanged, describe, check-attr, ls-tree, rev-parse", a.Subcommand),
		}
	}
	if a.Subcommand == "clean" && a.Args != "--dry-run" {
		return miniagent.ToolResult{IsError: true, Output: "git clean without --dry-run is blocked; use --dry-run to preview"}
	}
	dir, err := resolveGitRoot(workspaceRoot)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("not a git repository: %v", err)}
	}
	cmdArgs := []string{"-C", dir, "--no-pager", a.Subcommand}
	if a.Args != "" {
		cmdArgs = append(cmdArgs, strings.Fields(a.Args)...)
	}
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
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
	return "", fmt.Errorf("not a git repository")
}

func runLimitedOutput(ctx context.Context, cmd *exec.Cmd, maxOutputChars int) (string, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", err
	}
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
	<-waitErr
	killProcessGroup(cmd)
	return accum.finalize(maxOutputChars), nil
}
EOF