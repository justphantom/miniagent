package tools

import (
	"context"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"os/exec"
	"strings"
	"time"
)

type npmArgs struct {
	Subcommand string `json:"subcommand"`
	Args       string `json:"args,omitempty"`
}

// install/test/run 是 JS 生态开发的最小集：安装依赖、跑测试、执行 package.json scripts。
// publish/adduser/logout 等网络写操作排除；run 经 scripts 执行任意命令是 JS 生态常态，接受（1a 决策）。
var allowedNpmSubcommands = map[string]bool{
	"install": true, "ci": true, "test": true, "run": true,
	"ls": true, "outdated": true, "audit": true, "version": true,
}

func NpmTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "npm",
		Description: "Constrained npm for JS dev: " + sortedNames(allowedNpmSubcommands) + ". install/ci allow dependency sync (network write accepted). run executes package.json scripts (arbitrary commands by design). When the rtk proxy is deployed, output is compact and NOT native npm format. Timeout " + timeout.String() + "; non-zero exit (e.g. failing tests) is a normal result, not a tool failure.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "npm subcommand"},
			"args":       map[string]any{"type": "string", "description": `Additional arguments as ONE string; shell-style quoting keeps spaces intact. --prefix/-C/--registry are rejected`},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		// install/test 的错误摘要（ELIFECYCLE/exit 1）在输出尾部；与 shell/grep 同取 head+tail。
		SplitTruncate: true,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "npm", func(rctx context.Context) miniagent.ToolResult {
				return runNpm(rctx, workspaceRoot, args, maxOutputChars)
			})
		},
	}
}

func runNpm(ctx context.Context, workspaceRoot, args string, maxOutputChars int) miniagent.ToolResult {
	var a npmArgs
	if err := decodeStrict(args, &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed (args must be a JSON object with string fields subcommand/args, e.g. {\"subcommand\":\"test\"}): %v", err)}
	}
	sub := strings.TrimSpace(a.Subcommand)
	if sub == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedNpmSubcommands[sub] {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("npm %q is not allowed in default mode; use one of: %s", sub, sortedNames(allowedNpmSubcommands))}
	}
	fields, qerr := splitArgsStrict(a.Args)
	if qerr != "" {
		return miniagent.ToolResult{IsError: true, Output: "args " + qerr}
	}
	// --prefix/-C redirects npm's working root outside the module tree (out-of-subtree writes);
	// --registry overrides the registry endpoint (exfiltration of the dependency stream to an
	// attacker-controlled server that can serve malicious tarballs). .npmrc in workdir achieves the
	// same registry override — accepted residual (guardrail against misfired calls, not a boundary).
	for _, f := range fields {
		if strings.HasPrefix(f, "--prefix") || strings.HasPrefix(f, "-C") || strings.HasPrefix(f, "--registry") {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("npm %s option %q redirects npm outside the module or to another registry; blocked (default mode)", a.Subcommand, f)}
		}
	}
	cmdArgs := append([]string{sub}, fields...)
	bin, argv := rtkWrap("npm", []string{"npm"}, cmdArgs)
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = resolveModuleRoot(workspaceRoot)
	cmd.Env = scrubEnv(os.Environ())
	setPGID(cmd)
	body, err := runLimitedOutput(ctx, cmd, maxOutputChars)
	if err != nil {
		return exitAwareResult("npm", sub, body, err)
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}
