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
			Output:  fmt.Sprintf("go %q is not allowed in default mode; use one of: fmt, build, test, vet, doc, list, version, clean", a.Subcommand),
		}
	}
	fields := splitArgs(a.Args)
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
		bin, argv = rtkWrap("go", []string{"go", a.Subcommand}, fields)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = resolveModuleRoot(workspaceRoot)
	cmd.Env = scrubEnv(os.Environ())
	body, err := runLimitedOutput(ctx, cmd)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("go %s failed: %v\n%s", a.Subcommand, err, body)}
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}

// 拒绝写文件/写模块树之外/改源码的 go build/test 选项前缀。
// -o writes the build/test binary to an arbitrary path (outside the workdir subtree);
// -toolexec runs an arbitrary build-tool program during build/test — same class as -w/-modfile.
var deniedGoArgPrefixes = []string{"-w", "-write", "-fix", "-modfile", "-o", "-toolexec"}

// rtkGoSubcommands lists the subcommands that rtk go supports (compact output).
var rtkGoSubcommands = map[string]bool{"build": true, "test": true, "vet": true}

// resolveModuleRoot 从 startDir 向上找 go.mod，但**不越过 startDir**（default 模式）：
// 越界上溯会把 go/npm/lint 的 cwd 定到 workdir 外的父模块，模块级写（go.mod/go.sum/构建缓存
// 落点）越出子树。找不到时返回 startDir 本身，让 go 命令自行报错。
// 注：workdir 本身在模块子目录内（如 workdir=repo/cmd/x、go.mod 在 repo/）是常见布局——此时
// npm 在 workdir 找不到 package.json 会报错，属保守方向的已知代价；go 工具经 -C 语义不受影响。
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
	sep := string(filepath.Separator)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root — no go.mod anywhere up the chain
		}
		dir = parent
		if dir != orig && !strings.HasPrefix(dir, orig+sep) {
			// dir climbed ABOVE startDir (no longer startDir or a descendant): stop — startDir's
			// own go.mod was the first iteration's check. No upward escape (default mode).
			break
		}
	}
	return orig
}
