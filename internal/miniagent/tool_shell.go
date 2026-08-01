package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxShellOutputChars = 20000
const maxShellOutputBytes = maxShellOutputChars * 4
const shellTimeout = 60 * time.Second

// sudoSuRe 词边界匹配常见特权提升器（sudo/su/doas/pkexec/gsudo/run0）与专有特权/
// 命名空间工具（setpriv/nsenter/unshare/chroot/machinectl）：覆盖 "cd /x && sudo ..."
// 等中段命令。setpriv 等是低频专有工具，误伤远小于作为提权/逃逸工具的收益。仍可被
// 变量拼接/拆分/未列出的提权器绕过——default 是薄软约束，不构成安全边界
// （审查 v2 #10、P2-12、三轮 P3）。
// 不含 please：它是英文常用词（commit message/文件名常见），误伤远大于作为提权器的收益。
var sudoSuRe = regexp.MustCompile(`\b(sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl)\b`)

// ShellTool returns a shell tool bound to workspaceRoot. timeout<=0 用默认 shellTimeout。
// workspaceRoot 为空时 cmd.Dir 留空，exec 继承父进程 cwd。mode=default 时拒绝 sudo/su。
func ShellTool(workspaceRoot string, timeout time.Duration, mode string) Tool {
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
			if mode == ModeDefault && sudoSuRe.MatchString(a.Command) {
				return ToolResult{IsError: true, Output: "default 模式禁止特权提升器 sudo/su/doas/pkexec/gsudo/run0/setpriv/nsenter/unshare/chroot/machinectl（用 -mode auto 放行）"}
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
			cmd.Dir = workspaceRoot
			// 剥离 MINIAGENT_* 前缀与含密钥关键字的变量，降低 LLM 直接 echo 宿主
			// 配置/凭证的概率；非隔离边界，见 scrubEnv 注释。
			cmd.Env = scrubEnv(os.Environ())
			// 独立进程组：超时 kill(-pgid) 才能连带清理 sh 派生的孙子进程，
			// 否则 make/find 之类会成孤儿继续跑。
			setPGID(cmd)
			body, err := runShellLimited(runCtx, cmd)
			if err != nil {
				// 区分 shell 自身超时与父 ctx 取消：父 ctx（max-duration / 信号）未取消、
				// 仅 runCtx 到 deadline 时，才是 shell 自身超时；否则走通用失败分支，
				// 避免父超时被误报为「命令超时 >N」（runCtx 是 ctx 的子，父超时也会令 runCtx 到期）。
				if ctx.Err() == nil && runCtx.Err() != nil {
					return ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: body + fmt.Sprintf("\n⏱ 命令超时（>%s），已终止。", timeout)}
				}
				// 非 0 退出是命令的合法结果而非执行失败：提取退出码，IsError=false，
				// 让 LLM 据 ExitCode 判成败（旧版把非 0 退出当 IsError=true，语义不准）。
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					return ToolResult{Output: body, ExitCode: ee.ExitCode()}
				}
				return ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: body + fmt.Sprintf("\n执行失败：%v", err)}
			}
			return ToolResult{Output: body, ExitCode: 0}
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

// scrubEnv 复制 env 并移除：所有 MINIAGENT_* 前缀条目，以及变量名（大写后）含
// KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/PASSPHRASE/AUTH/PAT 的条目。后者覆盖 config
// 模式 ${MAIN_API_KEY} 注入的来源变量（非 MINIAGENT_ 前缀但同样承载真实 key），以及
// AWS_ACCESS_KEY_ID、GH_TOKEN/GITHUB_TOKEN/GITHUB_PAT/GITLAB_PAT、DATABASE_PASSWORD、
// MYSQL_PWD、DB_PASS/REDIS_PASS、GPG_PASSPHRASE、BASIC_AUTH/AUTH_HEADER 等高频宿主凭证，
// 降低非 -key-file 模式下 LLM echo 出密钥的概率。PWD/PASS/AUTH 等短关键字会扩大误伤面
// （如 AUTHPROXY、PASSWORDLESS 中含 PASS/PASSWORD）——安全侧倾斜的已知取舍，倾向过度
// 剥离而非泄漏。PAT 单独排除 PATH 族（PATH/PATHEXT/*_PATH），见 hasSecretKeyword 注释。
//
// 已知未覆盖（依赖 -key-file 与 OS 隔离兜底，不强制剥离以免误伤 agent 自身 shell 命令
// 所需环境）：DATABASE_URL/SERVICE_URL（URL 过宽，碰撞 SERVICE_URL 等良性变量）、
// *_COOKIE（较窄但低频）、*_DSN（同前）、*_CONN（碰撞 CONNECTION_*/CONNECT_* 过宽）。
// 这些是增量泄漏面收窄，不是密钥隔离边界——未列出的凭证名仍继承，且子进程可经
// /proc/$PPID/environ 读到 exec 前的完整环境快照。彻底方案是 -key-file（key 不进 env）。
// free 模式下隔离依赖调用方（容器/独立 UID），见 README。
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "MINIAGENT_") {
			continue
		}
		name := kv
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if hasSecretKeyword(strings.ToUpper(name)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasSecretKeyword 报告大写变量名是否含密钥相关关键字。仅服务于 scrubEnv，
// 与 MINIAGENT_ 前缀剥离互补；不构成完整凭证发现（见 scrubEnv 注释的隔离边界说明）。
// PWD/PASS/PASSPHRASE/AUTH 补全高频凭证名（MYSQL_PWD/DB_PASS/GPG_PASSPHRASE/BASIC_AUTH），
// 短关键字接受更大误伤面换安全侧倾斜（如 AUTHPROXY 被剥，可接受）。
// PAT 覆盖 GITHUB_PAT/GITLAB_PAT/AZURE_DEVOPS_EXT_PAT 等 fine-grained token（与已剥的
// GH_TOKEN/GITHUB_TOKEN 并存的常用名），但 PATH/PATHEXT/LD_LIBRARY_PATH/CPATH/GITHUB_PATH
// 等「路径」类变量共享 P-A-T 子串——PATH 是 shell 解析可执行路径的必需变量，误剥会让
// ls/grep/cat 全部失效（亦会破坏既有 TestScrubEnv 的 PATH 用例）。故 PAT 单独走「含 PATH
// 必为路径类、豁免」分支：GITHUB_PAT 不含 PATH（PAT 后无 H）→ 命中；GITHUB_PATH 含 PATH
// → 豁免。COMPAT_*/PATCH_* 等含 PAT 但不含 PATH 的罕见变量会被过度剥离，接受（同
// MONKEY_COUNT 取舍）。
func hasSecretKeyword(upperName string) bool {
	for _, kw := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PWD", "PASS", "PASSPHRASE", "AUTH"} {
		if strings.Contains(upperName, kw) {
			return true
		}
	}
	// PAT 单独判定：排除含 PATH 的路径类变量（PATH 不可剥，见上方注释）。
	return strings.Contains(upperName, "PAT") && !strings.Contains(upperName, "PATH")
}
