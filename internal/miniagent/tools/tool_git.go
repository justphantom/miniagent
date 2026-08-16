package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// sortedNames 与 map 同源生成逗号列表，避免手写串与 map 漂移（描述/错误共用的单一事实源）。
func sortedNames(m map[string]bool) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func GitTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "git",
		Description: "Git operations: read-only (status/diff/log/show/ls-tree etc) plus basic versioning (add/commit/pull/push). History-rewriting commands (reset/rebase/merge/checkout, commit --amend, push --force/--delete) and config/branch/tag management are blocked. push/pull operate on the configured remote only. Relative pathspecs resolve against the workdir, NOT the repo root. Commit requires -m (also accepts -am). When the rtk proxy is deployed, output is compact and NOT native git format. Timeout " + timeout.String() + "; non-zero exit is a normal result (see exit code), not a tool failure.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Git subcommand"},
			"args":       map[string]any{"type": "string", "description": `Additional arguments as ONE string; shell-style quoting keeps spaces intact (e.g. args: "-m \"feat: add thing\""). Options that write files or rewrite history are rejected`},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		// 错误结论（conflict/failed hunk/summary）在输出尾部，head 截断会丢失；与 shell/grep 同取 head+tail。
		SplitTruncate: true,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "git", func(rctx context.Context) miniagent.ToolResult {
				return runGit(rctx, workspaceRoot, args, maxOutputChars)
			})
		},
	}
}

func runGit(ctx context.Context, workspaceRoot, args string, maxOutputChars int) miniagent.ToolResult {
	var a gitArgs
	if err := decodeStrict(args, &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed (args must be a JSON object with string fields subcommand/args, e.g. {\"subcommand\":\"status\",\"args\":\"-m \\\"msg\\\"\"}): %v", err)}
	}
	sub := strings.TrimSpace(a.Subcommand)
	if sub == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedGitSubcommands[sub] {
		return miniagent.ToolResult{
			IsError: true,
			Output:  fmt.Sprintf("git %q is not in the allow-list; permitted: %s", sub, sortedNames(allowedGitSubcommands)),
		}
	}
	fields, qerr := splitArgsStrict(a.Args)
	if qerr != "" {
		return miniagent.ToolResult{IsError: true, Output: "args " + qerr}
	}
	if tok, spec, hit := checkDeniedOptions(fields, gitDeniedFor(sub)); hit {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("git %s option %q (%s) %s; blocked", sub, tok, spec.joinNames(), spec.reason)}
	}
	if err := checkGitPositionalArgs(sub, fields); err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	// commit without -m would open the configured editor (blocked non-interactively above → empty-message abort);
	// rejecting up front gives the LLM an actionable message instead of a confusing editor failure.
	if sub == "commit" && !hasGitMessageFlag(fields) {
		return miniagent.ToolResult{IsError: true, Output: `git commit requires -m "message" (or -am) in args`}
	}
	dir, err := resolveGitRoot(workspaceRoot)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("not a git repository: %v", err)}
	}
	if err := checkGitAttributes(ctx, dir); err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	// cwd 定 workdir（pathspec 与系统提示"相对路径基于 workdir"一致），不再 -C 仓库根——
	// 曾按 repo 根解析，子目录 workdir 下 add/diff 静默命中根下同名文件或假空 diff。
	cmdArgs := []string{"--no-pager", sub}
	cmdArgs = append(cmdArgs, fields...)
	bin, argv := "git", cmdArgs
	if rtkGitSubcommands[sub] {
		bin, argv = rtkWrap("git", []string{"git", "--no-pager", sub}, fields)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = workspaceRoot
	cmd.Env = append(scrubEnv(os.Environ()),
		// Non-interactive hardening: a repo with core.editor set (or EDITOR inherited) makes commit/tag
		// open an editor that blocks until the 120s timeout with no useful output. GIT_EDITOR=true makes
		// git accept the message as-is; GIT_TERMINAL_PROMPT=0 turns credential prompts into an immediate
		// error instead of a TTY hang; empty stdin makes a hook/prompt waiting on input see EOF.
		"GIT_EDITOR=true", "GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0",
	)
	if sub == "push" || sub == "pull" {
		// push/pull need remote auth (GITHUB_TOKEN/SSH_AUTH_SOCK were scrubbed as secret-named);
		// their output never echoes env, so restoring just these keeps the allow-listed workflow
		// functional — every other subcommand (incl. commit, which runs hooks) stays fully scrubbed.
		cmd.Env = restoreGitCredentials(cmd.Env, os.Environ())
	}
	cmd.Stdin = nil
	setPGID(cmd)
	body, err := runLimitedOutput(ctx, cmd, maxOutputChars)
	if err != nil {
		return exitAwareResult("git", sub, body, err)
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}

// exitAwareResult 区分「命令语义性非零退出」（正常结果，IsError=false + ExitCode，输出完整保留）
// 与「未能产出可信退出码」（启动失败/被信号杀死 → IsError=true）。与 shell 工具语义对齐：
// git diff --exit-code / go vet 有告警这类非零退出是探测结论，整段标 failed 会误导 LLM 判故障。
func exitAwareResult(tool, sub, body string, err error) miniagent.ToolResult {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		if body == "" {
			body = "(no output)\n"
		}
		return miniagent.ToolResult{Output: body, ExitCode: ee.ExitCode()}
	}
	return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: fmt.Sprintf("%s %s failed: %v\n%s", tool, sub, err, body)}
}

// hasGitMessageFlag reports whether args carry a commit message flag. Accepts -m/-am/-q and
// glued forms (-m"msg"、-m=msg、-mmsg) — git takes all of these; only -F (denied) reads a file.
func hasGitMessageFlag(fields []string) bool {
	for _, f := range fields {
		if f == "-m" || f == "--message" || strings.HasPrefix(f, "--message=") ||
			f == "-am" || f == "-a-m" || strings.HasPrefix(f, "-m") || strings.HasPrefix(f, "-am") {
			return true
		}
	}
	return false
}

// checkGitPositionalArgs rejects a repository URL where a refspec is expected. `git push <url> ...` /
// `git pull <url> ...` would target a remote other than the configured one — the exfiltration channel that
// remains after .git/config writes were blocked (the option check cannot see positional args).
// A refspec never contains "://", ":", or a path separator; anything that looks like a path
// (absolute, relative with / or \, or scp-style host:path) is rejected.
func checkGitPositionalArgs(subcommand string, fields []string) error {
	if subcommand != "push" && subcommand != "pull" {
		return nil
	}
	for _, f := range fields {
		if strings.HasPrefix(f, "-") {
			continue
		}
		if strings.ContainsAny(f, ":/\\") || filepath.IsAbs(f) || strings.Contains(f, "..") {
			return fmt.Errorf("git %s: %q looks like a repository URL/path, not a refspec; push/pull operate on the configured remote only (default mode)", subcommand, f)
		}
		break // first non-option positional is the only URL slot; the rest are refspecs
	}
	return nil
}

// checkGitAttributes rejects git operations whose clean/smudge filters or diff drivers would execute an
// external program: a workdir-writable .gitattributes declaring `filter=<name>` / `diff=<driver>` /
// `textconv=<cmd>` attributes turns `git add`/`git diff` into arbitrary-command execution — no .git access
// needed, so the .git lock does not cover it. Only drivers actually DEFINED (filter.<name>.clean /
// diff.<name>.command / textconv under [diff "<name>"] in git config) can execute; bare attribute tokens
// like `diff=java` (hunk-header only) are common and harmless, so an undefined driver passes. Guardrail,
// not a boundary — incoming attributes via pull are supply-chain exposure by definition and are not
// pre-checkable.
func checkGitAttributes(ctx context.Context, dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	if err != nil {
		//nolint:nilerr // absent/unreadable attributes file: nothing declared, not an error
		return nil
	}
	var declared []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for tok := range strings.FieldsSeq(line) {
			name, value, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			if v := strings.TrimSpace(value); v != "" && (name == "filter" || name == "diff" || name == "textconv") {
				declared = append(declared, v)
			}
		}
	}
	if len(declared) == 0 {
		return nil
	}
	defined, err := definedGitDrivers(ctx, dir)
	if err != nil {
		// git config 不可读时保守拒绝：驱动属性在场而无法证伪。
		return fmt.Errorf(".gitattributes declares external driver(s) %v but git config could not be read to verify them: %w (default mode)", declared, err)
	}
	for _, d := range declared {
		if defined[d] {
			return fmt.Errorf(".gitattributes declares external driver %q which is defined in git config (filter/diff/textconv execute commands; default mode) — remove the line or use -mode auto", d)
		}
	}
	return nil
}

// definedGitDrivers 解析 `git config -l --null` 输出，返回已定义的 filter.<name>.* 与 diff.<name>.*
// 驱动名集合。-l 含 system/global/local 三级，覆盖驱动定义的所有来源。
func definedGitDrivers(ctx context.Context, dir string) (map[string]bool, error) {
	cfgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cfgCtx, "git", "-C", dir, "config", "-l", "--null", "--includes")
	cmd.Env = scrubEnv(os.Environ())
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	defined := map[string]bool{}
	for entry := range strings.SplitSeq(out.String(), "\x00") {
		key, _, ok := strings.Cut(entry, "\n")
		if !ok {
			key = entry
		}
		key = strings.ToLower(key)
		// filter.<name>.clean/smudge → name；diff.<name>.command/textconv → name（diff.textconv 顶层键无驱动语义，跳过）
		for _, prefix := range []string{"filter.", "diff."} {
			if strings.HasPrefix(key, prefix) {
				rest := key[len(prefix):]
				if dot := strings.IndexByte(rest, '.'); dot > 0 {
					defined[rest[:dot]] = true
				}
			}
		}
	}
	return defined, nil
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
