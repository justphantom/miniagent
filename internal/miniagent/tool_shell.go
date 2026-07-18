package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxShellOutputChars = 20000
const maxShellOutputBytes = maxShellOutputChars * 4
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

// allowedEnvVars 是 shell 子进程允许继承的环境变量白名单。
var allowedEnvVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true,
	"PWD": true, "OLDPWD": true, "TMPDIR": true, "TZ": true,
	"EDITOR": true, "PAGER": true, "GOPATH": true, "GOROOT": true,
	"CGO_ENABLED": true, "GOFLAGS": true, "GOOS": true, "GOARCH": true,
}

// ShellTool returns a shell tool bound to workspaceRoot.
func ShellTool(workspaceRoot string, unrestricted bool, blockedPatterns []string) Tool {
	return Tool{
		Name:        "shell",
		Description: "在 workspace_root 下执行一条 shell 命令（sh -c）。返回 stdout+stderr 合并输出。命令最长运行 " + shellTimeout.String() + "；输出超过 " + fmt.Sprintf("%d", maxShellOutputChars) + " 字符会被截断。",
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
				if reason := validateShellCommand(a.Command, blockedPatterns); reason != "" {
					return ToolResult{IsError: true, Output: reason}
				}
			}
			runCtx, cancel := context.WithTimeout(ctx, shellTimeout)
			defer cancel()
			cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
			cmd.Dir = workspaceRoot
			// unrestricted 模式下 cmd.Dir 可能为空（继承进程 cwd），由 exec 自行处理；
			// 受限模式必须保证 workspace_root 可访问。
			if !unrestricted {
				if err := ensureWorkspaceDir(cmd.Dir); err != nil {
					return ToolResult{IsError: true, Output: err.Error()}
				}
			}
			cmd.Env = envWhitelist()
			// 独立进程组：超时 kill(-pgid) 才能连带清理 sh 派生的孙子进程，
			// 否则 make/find 之类会成孤儿继续跑。
			setPGid(cmd)
			body, err := runShellLimited(runCtx, cmd)
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

func runShellLimited(ctx context.Context, cmd *exec.Cmd) (string, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", err
	}
	// exec 不会主动关闭 io.PipeWriter；ctx 超时后主进程已被 CommandContext
	// 杀掉，但 pw 仍开着，io.Copy 会永久阻塞。这里监听 ctx，一旦 done 就关闭
	// pw 并 kill 整组，让 io.Copy 解除阻塞。
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
	var out bytes.Buffer
	limited := io.LimitReader(pr, maxShellOutputBytes)
	_, _ = io.Copy(&out, limited)
	err := <-waitErr
	// 兜底：正常退出后也整组清理一次，防后台 & 残留。
	killProcessGroup(cmd)
	return truncate(out.String(), maxShellOutputChars, "…"), err
}

func ensureWorkspaceDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("shell 未配置：workspace_root 为空")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("解析 workspace_root 失败：%v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("workspace_root %q 不可访问：%v", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace_root %q 不是目录", dir)
	}
	return nil
}

// validateShellCommand 是辅助过滤层，不是安全边界：黑名单本质可被绕过
// （rm --recursive、$()、find -delete 等），真正的隔离依赖 workspace_root
// 与 unrestricted=false 的路径约束。
func validateShellCommand(command string, blockedPatterns []string) string {
	folded := strings.ToLower(command)
	// len==0 而非 nil：用户传空 JSON 数组 [] 时也应回落默认值，
	// 否则会静默放开全部破坏性命令。
	patterns := blockedPatterns
	if len(patterns) == 0 {
		patterns = defaultBlockedPatterns
	}
	for _, p := range patterns {
		if p != "" && strings.Contains(folded, strings.ToLower(p)) {
			return fmt.Sprintf("拒绝执行：命令匹配黑名单模式 %q（破坏性命令已被拦截）。", p)
		}
	}
	// 拦截常见的命令注入/混淆模式，防止黑名单被绕过。
	for _, d := range []string{
		"| sh", "|sh", "| bash", "|bash", "| /bin/sh", "| /bin/bash",
		"base64 -d |", "base64 --decode |", "base64 -d|", "base64 --decode|",
		"source /dev/stdin", ". /dev/stdin",
	} {
		if strings.Contains(folded, d) {
			return fmt.Sprintf("拒绝执行：命令包含危险模式 %q（可能被用于绕过黑名单）。", d)
		}
	}
	return ""
}

func envWhitelist() []string {
	out := make([]string, 0, len(allowedEnvVars))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if allowedEnvVars[name] {
			out = append(out, kv)
		}
	}
	return out
}
