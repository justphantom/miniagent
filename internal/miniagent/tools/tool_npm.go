package tools

import (
	"context"
	"encoding/json"
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

func NpmTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	return miniagent.Tool{
		Name:        "npm",
		Description: "Constrained npm for JS dev: install/ci/test/run/ls/outdated/audit. install/ci allow dependency sync (network write accepted). run executes package.json scripts (arbitrary commands by design).",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "npm subcommand"},
			"args":       map[string]any{"type": "string", "description": "Additional arguments (whitespace-split)"},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "npm", func(rctx context.Context) miniagent.ToolResult {
				return runNpm(rctx, workspaceRoot, args)
			})
		},
	}
}

func runNpm(ctx context.Context, workspaceRoot, args string) miniagent.ToolResult {
	var a npmArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v", err)}
	}
	if strings.TrimSpace(a.Subcommand) == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedNpmSubcommands[a.Subcommand] {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("npm %q is not allowed in default mode; use one of: install, ci, test, run, ls, outdated, audit, version", a.Subcommand)}
	}
	fields := strings.Fields(a.Args)
	cmdArgs := append([]string{a.Subcommand}, fields...)
	cmd := exec.CommandContext(ctx, "npm", cmdArgs...)
	cmd.Dir = resolveModuleRoot(workspaceRoot)
	cmd.Env = scrubEnv(os.Environ())
	setPGID(cmd)
	body, err := runLimitedOutput(ctx, cmd, maxShellOutputChars)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("npm %s failed: %v\n%s", a.Subcommand, err, body)}
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}
