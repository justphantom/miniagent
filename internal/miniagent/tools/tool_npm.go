package tools

import (
	"context"
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

// npmDeniedOptions：越出模块树（--prefix/-C）或改写 registry 端点（--registry）的选项。
// matchOption 对单/双破折号归一化，-registry=URL 与 --registry=URL 同 hit；-C 以单字母长名
// 列入 longs（而非 shorts）——npm 实测双横线 --C 同样重定向前缀，shorts 分支不认双横线形。
var npmDeniedOptions = []optSpec{
	{longs: []string{"prefix", "registry", "C"}, reason: reasonOutOfTree},
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
		return denyResult("argument parsing failed (args must be a JSON object with string fields subcommand/args, e.g. {\"subcommand\":\"test\"}): %v", err)
	}
	sub := strings.TrimSpace(a.Subcommand)
	if sub == "" {
		return denyResult("missing argument: subcommand")
	}
	if !allowedNpmSubcommands[sub] {
		return denyResult("npm %q is not allowed in default mode; use one of: %s", sub, sortedNames(allowedNpmSubcommands))
	}
	fields, qerr := splitArgsStrict(a.Args)
	if qerr != "" {
		return denyResult("args %s", qerr)
	}
	if op := checkShellMetachars(a.Args); op != "" {
		return denyResult("%s", denyShellMetachars(a.Args))
	}
	fields = stripDupSubcommand(sub, fields)
	// --prefix/-C redirects npm's working root outside the module tree (out-of-subtree writes);
	// --registry overrides the registry endpoint (exfiltration of the dependency stream to an
	// attacker-controlled server that can serve malicious tarballs). npm 接受单破折号长名
	// （-registry=URL 实测等价），故用 optSpec 归一化匹配而非手写 HasPrefix（后者漏单破折形）。
	// .npmrc in workdir achieves the same registry override — accepted residual (guardrail against
	// misfired calls, not a boundary).
	if tok, spec, hit := checkDeniedOptions(fields, npmDeniedOptions); hit {
		return denyResult("npm %s option %q (%s) redirects npm outside the module or to another registry; blocked (default mode)", sub, tok, spec.joinNames())
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
