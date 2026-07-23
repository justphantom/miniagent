package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const maxShellOutputChars = 20000
const maxShellOutputBytes = maxShellOutputChars * 4
const shellTimeout = 60 * time.Second

// ShellTool returns a shell tool bound to workspaceRoot.
// workspaceRoot 为空时 cmd.Dir 留空，exec 继承父进程 cwd。
func ShellTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "shell",
		Description: "通过 sh -c 执行一条 shell 命令。返回 stdout+stderr 合并输出。命令最长运行 " + shellTimeout.String() + "；输出超过 " + strconv.Itoa(maxShellOutputChars) + " 字符会被截断。",
		Parameters: object(map[string]any{
			"command": map[string]any{"type": "string", "description": "要执行的 shell 命令"},
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
			runCtx, cancel := context.WithTimeout(ctx, shellTimeout)
			defer cancel()
			cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
			cmd.Dir = workspaceRoot
			// cmd.Env 不设：子进程继承父进程全量环境。
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
