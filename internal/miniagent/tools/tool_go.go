package tools

import (
	"context"
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

func GoTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "go",
		Description: "Constrained go operations for building and testing (" + sortedNames(allowedGoSubcommands) + "; clean may NOT touch -cache/-modcache/-testcache/-fuzzcache/-i). run/get/install/mod/env -w are blocked, as are -exec/-vettool/-toolexec/-C and out-of-tree write flags (-o/-coverprofile/-memprofile … accept workdir-relative paths only). Note: private-module auth (GOPRIVATE) is not available in default mode. When the rtk proxy is deployed, build/test/vet output is compact and NOT native go format. Timeout " + timeout.String() + "; non-zero exit (e.g. test failure) is a normal result, not a tool failure.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Go subcommand"},
			"args":       map[string]any{"type": "string", "description": `Additional arguments as ONE string; shell-style quoting keeps spaces intact. Out-of-tree options are rejected`},
		}, "subcommand"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		// test/build 的 FAIL 明细在输出尾部，head 截断只剩包列表；与 shell/grep 同取 head+tail。
		SplitTruncate: true,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "go", func(rctx context.Context) miniagent.ToolResult {
				return runGo(rctx, workspaceRoot, args, maxOutputChars)
			})
		},
	}
}

// goExecDeniedOptions：执行外部程序的构建期选项，任何子命令都拒。
// -toolexec build/test、-vettool vet、-exec test（用指定程序包装/运行测试二进制）。
var goExecDeniedOptions = []optSpec{
	{longs: []string{"toolexec", "vettool", "exec"}, reason: reasonExec},
}

// goGlobalDeniedOptions：所有子命令都拒的写类/越界类选项。
var goGlobalDeniedOptions = []optSpec{
	// -w 写 go env 配置（--write 等价长形）；-modfile 换用任意 go.mod（模块树外读写）；-fix 改源码。
	{longs: []string{"w", "write", "modfile", "fix"}, reason: reasonWrite},
	// -C 先 chdir 再执行：任何后续路径判定（含 deny 表的 workdir 相对豁免）都被整体搬走，唯一安全处理是全局禁。
	{longs: []string{"C"}, reason: reasonOutOfTree},
}

// goCleanDeniedOptions：clean 专属——全局缓存/安装产物在模块树外，误清不应惩罚宿主构建状态。
// -cache/-modcache/-testcache/-fuzzcache 全覆盖（长选项唯一缩写由 matchOption 处理）。
var goCleanDeniedOptions = []optSpec{
	{longs: []string{"cache", "modcache", "testcache", "fuzzcache", "i"}, reason: reasonOutOfTree},
}

// goWritePathFlags：build/test 会按参数值写文件的旗标。-o 等产物、-coverprofile/-memprofile 等
// profile、-trace；这些旗标本身合法（AGENTS 要求二进制落 bin/），只对「值在 workdir 子树外」拒绝：
// 绝对路径须在 workdir 内，相对路径不得以 ../ 开头。-outputdir 被 -o 前缀规则顺带覆盖（合法，
// 因其值同样必须是输出目录）。值既可独立 token（下一位）也可 =value 粘合。
var goWritePathFlags = []string{"-o", "-coverprofile", "-memprofile", "-cpuprofile", "-blockprofile", "-mutexprofile", "-trace"}

func runGo(ctx context.Context, workspaceRoot, args string, maxOutputChars int) miniagent.ToolResult {
	var a goArgs
	if err := decodeStrict(args, &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed (args must be a JSON object with string fields subcommand/args, e.g. {\"subcommand\":\"test\",\"args\":\"./...\"}): %v", err)}
	}
	sub := strings.TrimSpace(a.Subcommand)
	if sub == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedGoSubcommands[sub] {
		return miniagent.ToolResult{
			IsError: true,
			Output:  fmt.Sprintf("go %q is not allowed in default mode; use one of: %s", sub, sortedNames(allowedGoSubcommands)),
		}
	}
	fields, qerr := splitArgsStrict(a.Args)
	if qerr != "" {
		return miniagent.ToolResult{IsError: true, Output: "args " + qerr}
	}
	specs := append(append([]optSpec{}, goGlobalDeniedOptions...), goExecDeniedOptions...)
	if sub == "clean" {
		specs = append(specs, goCleanDeniedOptions...)
	}
	if tok, spec, hit := checkDeniedOptions(fields, specs); hit {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("go %s option %q (%s) %s; blocked", sub, tok, spec.joinNames(), spec.reason)}
	}
	if err := checkGoWritePaths(sub, fields, workspaceRoot); err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	cmdArgs := []string{sub}
	cmdArgs = append(cmdArgs, fields...)
	bin, argv := "go", cmdArgs
	if rtkGoSubcommands[sub] {
		bin, argv = rtkWrap("go", []string{"go", sub}, fields)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = resolveModuleRoot(workspaceRoot)
	cmd.Env = scrubEnv(os.Environ())
	setPGID(cmd)
	body, err := runLimitedOutput(ctx, cmd, maxOutputChars)
	if err != nil {
		return exitAwareResult("go", sub, body, err)
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}

// checkGoWritePaths 对 goWritePathFlags 的每个出现：取值（=value 粘合或下一位 token），
// 绝对路径须在 workdir 子树内、相对路径不得 ../ 越出；两者之外的写路径拒绝。
// 缺值（旗标是末位 token）不在此处理，交给 go 本体报错。
func checkGoWritePaths(sub string, fields []string, workdir string) error {
	if sub != "build" && sub != "test" {
		return nil
	}
	for i, f := range fields {
		var name, value string
		hit := false
		for _, p := range goWritePathFlags {
			if f == p {
				name, hit = p, true
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					value = fields[i+1]
				}
				break
			}
			if strings.HasPrefix(f, p+"=") {
				name, value, hit = p, f[len(p)+1:], true
				break
			}
		}
		if !hit || value == "" {
			continue
		}
		if filepath.IsAbs(value) {
			if !withinDir(workdir, value) {
				return fmt.Errorf("go %s option %s %q writes outside the workdir; blocked (use a workdir-relative path)", sub, name, value)
			}
			continue
		}
		if strings.HasPrefix(value, "../") || value == ".." {
			return fmt.Errorf("go %s option %s %q writes outside the workdir; blocked (use a workdir-relative path)", sub, name, value)
		}
	}
	return nil
}

// withinDir 报告 path（绝对）是否在 root（绝对）子树内或等于 root。
func withinDir(root, path string) bool {
	sep := string(filepath.Separator)
	return path == root || strings.HasPrefix(path+sep, root+sep)
}

// rtkGoSubcommands lists the subcommands that rtk go supports (compact output).
var rtkGoSubcommands = map[string]bool{"build": true, "test": true, "vet": true}

// resolveModuleRoot 定位 go 命令的 cwd：workdir 有 go.mod 用 workdir；否则交给 go 二进制自身的
// 向上查找（go -C 语义不变，cmd.Dir 只决定起点）。default 模式下 npm/lint 同用此函数：
// workdir 为模块子目录时 npm 找不到 package.json 属保守方向的已知代价。
// （旧版「向上找但不得越出 startDir」的循环：dir 只会向上走，第二轮起必触发 break，
// 等价于只查 startDir——死代码，已删。）
func resolveModuleRoot(startDir string) string {
	dir := startDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "."
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return dir
	}
	return dir
}
