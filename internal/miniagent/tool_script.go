package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// shellQuote 把参数字符串按 POSIX shell 单引号规则转义，防止追加到命令时发生注入。
// 仅当 args 含特殊字符时才加引号；普通连续字符保持原样。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\";'|&`$()<>*?[]{}~#\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// ScriptTool 把一条固定命令封装为工具（P1：.miniagent/scripts.json 注册的项目专用工具）。
// name 自动加 script_ 前缀避免与内置工具冲突；description 来自 scripts.json。
// 可选 args（字符串）追加到 command 末尾（空格分隔），复用 runShellCommand 的安全策略
// （mode 黑名单 / env 剥离 / 超时 / 进程组 / 输出截断）。继承 default 模式约束。
func ScriptTool(name, description, command, workdir string, timeout time.Duration, mode string, maxOutputChars, streamWindow int) Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	return Tool{
		Name:        "script_" + name,
		Description: "项目脚本：" + description + "（执行固定命令，最长运行 " + timeout.String() + "）",
		Parameters: object(map[string]any{
			"args": map[string]any{"type": "string", "description": "可选：追加到脚本命令末尾的参数（空格分隔）"},
		}),
		ResultLimit:   maxToolResultInHistory,
		SplitTruncate: true, // 复用 runShellCommand，输出语义同 shell（错误结论在尾部）
		Call: func(ctx context.Context, args string) ToolResult {
			var a struct {
				Args string `json:"args,omitempty"`
			}
			// args 可空（LLM 调用脚本常不传参）；非空才解析，解析失败返回明确错误。
			if strings.TrimSpace(args) != "" {
				if err := json.Unmarshal([]byte(args), &a); err != nil {
					return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
				}
			}
			full := command
			if trimmed := strings.TrimSpace(a.Args); trimmed != "" {
				// 拒 `-` 开头：shellQuote 只防 shell 注入，挡不住 argv 层 flag 注入到固定命令
				//（如 script_git（git push）+ args --force → git push --force）。scripts.json 本意约束固定命令。
				if trimmed[0] == '-' {
					return ToolResult{IsError: true, Output: "脚本工具参数不能以 - 开头（防向固定命令注入 flag）"}
				}
				full = command + " " + shellQuote(trimmed)
			}
			return runShellCommand(ctx, workdir, mode, full, timeout, maxOutputChars, streamWindow)
		},
	}
}
