package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const maxShellOutputChars = 20000
const maxShellOutputBytes = maxShellOutputChars * 4
const shellTimeout = 60 * time.Second

// ShellTool returns a shell tool bound to workspaceRoot. timeout<=0 用默认
// shellTimeout。workspaceRoot 为空时 cmd.Dir 留空，exec 继承父进程 cwd。
func ShellTool(workspaceRoot string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	return Tool{
		Name:        "shell",
		Description: "通过 sh -c 执行一条 shell 命令。返回 stdout+stderr 合并输出。命令最长运行 " + timeout.String() + "；输出超过 " + strconv.Itoa(maxShellOutputChars) + " 字符会被截断。",
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
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
			cmd.Dir = workspaceRoot
			// 仅剥离 MINIAGENT_* 前缀变量，降低 LLM 直接 echo 宿主配置的概率；
			// 非隔离边界，见 scrubEnv 注释。
			cmd.Env = scrubEnv(os.Environ())
			// 独立进程组：超时 kill(-pgid) 才能连带清理 sh 派生的孙子进程，
			// 否则 make/find 之类会成孤儿继续跑。
			setPGID(cmd)
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
	n, _ := io.Copy(&out, limited)
	if n >= maxShellOutputBytes {
		// 输出达上限：LimitReader 已 EOF，但子进程仍向写满的 pipe 继续写而阻塞，
		// cmd.Wait 不返回、空等至 60s 超时。主动 kill 整组并关 pipe，让子进程
		// 收 SIGPIPE/被杀后 Wait 立即返回，避免高输出命令被误判为"超时"。
		killProcessGroup(cmd)
		_ = pw.Close()
	}
	err := <-waitErr
	// 兜底：正常退出后也整组清理一次，防后台 & 残留。
	killProcessGroup(cmd)
	return truncate(out.String(), maxShellOutputChars, "…"), err
}

// scrubEnv 复制 env 并移除所有 MINIAGENT_* 前缀条目（API_KEY/BASE_URL 等）。
// 这不是密钥隔离边界：其他第三方变量原样继承，且子进程可经
// /proc/<ppid>/environ 读到 exec 前的环境。free 模式下隔离依赖调用方
// （容器/独立 UID），见 README。
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "MINIAGENT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
