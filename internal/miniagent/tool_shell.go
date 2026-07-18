package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxShellOutputChars = 20000
const shellTimeout = 60 * time.Second

var defaultBlockedPatterns = []string{
	"rm -rf",
	"rm -fr",
	"rm -r -f",
	"rm -f -r",
	"rm -r /",
	"rm -R ",
	"mkfs",
	"dd if=",
	"shutdown",
	"poweroff",
	"reboot",
	"halt",
	":(){:|:&};:",
	"> /dev/sd",
	"chmod -R 000",
	"chown -R",
}

var secretEnvPrefixes = []string{
	"MINIAGENT_",
	"FEISHU_",
	"IPC_",
}

// ShellTool returns a shell tool bound to workspaceRoot.
func ShellTool(workspaceRoot string, unrestricted bool, blockedPatterns []string) Tool {
	return Tool{
		Name:        "shell",
		Description: "在 workspace_root 下执行一条 shell 命令（sh -c）。返回 stdout+stderr 合并输出。破坏性命令会被拒绝；命令最长运行 " + shellTimeout.String() + "。",
		Parameters: object(map[string]any{
			"command": map[string]any{"type": "string", "description": "要执行的 shell 命令，相对路径基于 workspace_root"},
		}, "command"),
		Call: func(ctx context.Context, args string) ToolResult {
			var a struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if strings.TrimSpace(a.Command) == "" {
				return ToolResult{IsError: true, Output: "参数缺失：command"}
			}
			if !unrestricted {
				if strings.TrimSpace(workspaceRoot) == "" {
					return ToolResult{IsError: true, Output: "shell 未配置：workspace_root 为空"}
				}
				patterns := blockedPatterns
				if len(patterns) == 0 {
					patterns = defaultBlockedPatterns
				}
				if msg := blockedShellReason(a.Command, patterns); msg != "" {
					return ToolResult{IsError: true, Output: msg}
				}
			}
			runCtx, cancel := context.WithTimeout(ctx, shellTimeout)
			defer cancel()
			cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
			if workspaceRoot != "" {
				root, err := filepath.Abs(workspaceRoot)
				if err == nil {
					if _, statErr := os.Stat(root); statErr == nil {
						cmd.Dir = root
					}
				}
			}
			cmd.Env = envWithoutSecrets()
			out, err := cmd.CombinedOutput()
			body := truncate(string(out), maxShellOutputChars, "…")
			if err != nil {
				if runCtx.Err() == context.DeadlineExceeded {
					return ToolResult{IsError: true, Output: body + fmt.Sprintf("\n⏱ 命令超时（>%s），已终止。", shellTimeout)}
				}
				return ToolResult{IsError: true, Output: body + fmt.Sprintf("\n退出码错误：%v", err)}
			}
			return ToolResult{Output: body}
		},
	}
}

func blockedShellReason(command string, patterns []string) string {
	folded := strings.ToLower(command)
	for _, p := range patterns {
		if strings.Contains(folded, strings.ToLower(p)) {
			return fmt.Sprintf("拒绝执行：命令匹配黑名单模式 %q（破坏性命令已被拦截）。", p)
		}
	}
	return ""
}

func envWithoutSecrets() []string {
	out := make([]string, 0, 64)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || isSecretEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isSecretEnv(name string) bool {
	for _, p := range secretEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	upper := strings.ToUpper(name)
	switch {
	case strings.HasSuffix(upper, "_SECRET"),
		strings.HasSuffix(upper, "_KEY"),
		strings.HasSuffix(upper, "_TOKEN"),
		strings.HasSuffix(upper, "_PASSWORD"),
		strings.HasSuffix(upper, "_API_KEY"):
		return true
	}
	return false
}
