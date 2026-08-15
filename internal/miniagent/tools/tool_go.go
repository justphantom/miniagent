package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type goArgs struct {
	Subcommand string `json:"subcommand"`
	Args       string `json:"args,omitempty"`
}

// 开发测试所需的最小集：格式化、编译、测试、静态检查、文档、列举、版本。
// run 等同 shell 执行任意代码、bug 打开浏览器、info 非标准命令，均排除（收紧原则）。
// fmt 写入模块树内 .go 文件（verify-gate 首步 gofmt 的等价物），无越界写风险。
var allowedGoSubcommands = map[string]bool{
	"fmt": true, "build": true, "test": true, "vet": true,
	"doc": true, "list": true, "version": true, "clean": true,
}

func GoTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	return miniagent.Tool{
		Name:        "go",
		Description: "Constrained go operations for building and testing (build/test/vet/doc/list/version/clean). run/get/install/mod/env-w are blocked.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Go subcommand"},
			"args":       map[string]any{"type": "string", "description": "Additional arguments (whitespace-split)"},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "go", func(rctx context.Context) miniagent.ToolResult {
				return runGo(rctx, workspaceRoot, args)
			})
		},
	}
}

func runGo(ctx context.Context, workspaceRoot, args string) miniagent.ToolResult {
	var a goArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v", err)}
	}
	if strings.TrimSpace(a.Subcommand) == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedGoSubcommands[a.Subcommand] {
		return miniagent.ToolResult{
			IsError: true,
			Output:  fmt.Sprintf("go %q is not allowed in default mode; use one of: build, test, vet, doc, list, version, clean", a.Subcommand),
		}
	}
	fields := strings.Fields(a.Args)
	for _, f := range fields {
		for _, p := range deniedGoArgPrefixes {
			if strings.HasPrefix(f, p) {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("go %s option %q writes outside the module tree or edits files; blocked", a.Subcommand, f)}
			}
		}
	}
	cmdArgs := []string{a.Subcommand}
	cmdArgs = append(cmdArgs, fields...)
	bin, argv := "go", cmdArgs
	if rtkGoSubcommands[a.Subcommand] {
		bin, argv = rtkWrap("go", []string{"go", a.Subcommand}, cmdArgs)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = resolveModuleRoot(workspaceRoot)
	cmd.Env = scrubEnv(os.Environ())
	body, err := runLimitedOutput(ctx, cmd, maxShellOutputChars)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("go %s failed: %v\n%s", a.Subcommand, err, body)}
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}

// 拒绝写文件/写模块树之外/改源码的 go build/test 选项前缀。
var deniedGoArgPrefixes = []string{"-w", "-write", "-fix", "-modfile"}

// rtkGoSubcommands lists the subcommands that rtk go supports (compact output).
var rtkGoSubcommands = map[string]bool{"build": true, "test": true, "vet": true}

// resolveModuleRoot 从 startDir 向上找 go.mod；找不到时返回 startDir 本身，
// 让 go 命令自行报错（此前误返回字面量 "."，会跑到进程 cwd 上执行）。
func resolveModuleRoot(startDir string) string {
	dir := startDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "."
		}
	}
	orig := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return orig
}
