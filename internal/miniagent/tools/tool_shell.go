package tools

import (
	"github.com/justphantom/miniagent/internal/miniagent/policy"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
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

// maxShellOutputChars 是 shell/glob/grep 共享的工具输出字符上限：100KB 覆盖典型命令输出。
// 运行时经 miniagent.Limits.MaxShellOutputChars 覆盖（<=0 用此默认）。streamWindow 默认 = maxOutputChars*8。
const maxShellOutputChars = 100000

const shellTimeout = 120 * time.Second

// sudoSuRe 词边界匹配常见特权提升器（sudo/su/doas/pkexec/gsudo/run0）与专有特权/
// 命名空间工具（setpriv/nsenter/unshare/chroot/machinectl）：覆盖 "cd /x && sudo ..."
// 等中段命令。setpriv 等是低频专有工具，误伤远小于作为提权/逃逸工具的收益。仍可被
// 变量拼接/拆分/未列出的提权器绕过——default 是薄软约束，不构成安全边界
// （审查 v2 #10、P2-12、三轮 P3）。不含 please：它是英文常用词，误伤远大于收益。
var sudoSuRe = regexp.MustCompile(`\b(sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl)\b`)

// ShellTool returns a shell tool bound to workspaceRoot. timeout<=0 用默认 shellTimeout。
// workspaceRoot 为空时 cmd.Dir 留空，exec 继承父进程 cwd。mode=default 时拒绝 sudo/su。
func ShellTool(workspaceRoot string, timeout time.Duration, mode string, maxOutputChars, streamWindow int) miniagent.Tool {
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	if streamWindow <= 0 {
		streamWindow = maxOutputChars * 8
	}
	return miniagent.Tool{
		Name:        "shell",
		Description: "通过 sh -c 执行一条 shell 命令。返回 stdout+stderr 合并输出。命令最长运行 " + timeout.String() + "；输出超过 " + strconv.Itoa(maxOutputChars) + " 字符会被截断。",
		Parameters: object(map[string]any{
			"command": map[string]any{"type": "string", "description": "要执行的 shell 命令"},
		}, "command"),
		ResultLimit:   policy.MaxToolResultInHistory,
		SplitTruncate: true, // shell 输出的错误结论（exit status / FAIL）常在尾部，前截断会丢失
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			var a struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if strings.TrimSpace(a.Command) == "" {
				return miniagent.ToolResult{IsError: true, Output: "参数缺失：command"}
			}
			return runShellCommand(ctx, workspaceRoot, mode, a.Command, timeout, maxOutputChars, streamWindow)
		},
	}
}

// runShellCommand 执行 command（经 sh -c），含 mode 黑名单/env 剥离/进程组/超时/输出截断/退出码映射。
// shell 工具与 script 工具共用（P1：.miniagent/scripts.json 注册的工具继承同一套安全策略）。
// timeout<=0 用默认 shellTimeout。区分 shell 自身超时与父 ctx 取消：父 ctx 未取消、仅 runCtx 到期
// 才是 shell 自身超时；非 0 退出是命令的合法结果（IsError=false，LLM 据 ExitCode 判成败）。
func runShellCommand(ctx context.Context, workdir, mode, command string, timeout time.Duration, maxOutputChars, streamWindow int) miniagent.ToolResult {
	if mode == miniagent.ModeDefault && sudoSuRe.MatchString(command) {
		return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: "default 模式禁止特权提升器 sudo/su/doas/pkexec/gsudo/run0/setpriv/nsenter/unshare/chroot/machinectl（用 -mode auto 放行）"}
	}
	if timeout <= 0 {
		timeout = shellTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	if streamWindow <= 0 {
		streamWindow = maxOutputChars * 8
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = workdir
	// 剥离 MINIAGENT_* 前缀与含密钥关键字的变量，降低 LLM 直接 echo 宿主
	// 配置/凭证的概率；非隔离边界，见 scrubEnv 注释。
	cmd.Env = scrubEnv(os.Environ())
	// 独立进程组：超时 kill(-pgid) 才能连带清理 sh 派生的孙子进程，
	// 否则 make/find 之类会成孤儿继续跑。
	setPGID(cmd)
	body, err := runShellLimited(runCtx, cmd, maxOutputChars, streamWindow)
	if err != nil {
		// runCtx 是 ctx 的子，父超时也会令 runCtx 到期；仅父 ctx 未取消时才算 shell 自身超时。
		if ctx.Err() == nil && runCtx.Err() != nil {
			return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\n⏱ 命令超时（>%s），已终止。", timeout)}
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() < 0 {
				// 信号杀死（含父 ctx 取消的 SIGKILL、shell 超时后的进程组清理）：非命令合法退出，
				// 按 IsError + miniagent.ExitCodeNotSet 记——守住「miniagent.ExitCodeNotSet ⟺ IsError」约定（与超时/取消一致），
				// 避免 LLM 看到 IsError=false + 负退出码的矛盾结果。
				return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\n命令被信号终止：%v。", ee)}
			}
			return miniagent.ToolResult{Output: body, ExitCode: ee.ExitCode()}
		}
		return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: body + fmt.Sprintf("\n执行失败：%v", err)}
	}
	return miniagent.ToolResult{Output: body, ExitCode: 0}
}

func runShellLimited(ctx context.Context, cmd *exec.Cmd, maxOutputChars, streamWindow int) (string, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", err
	}
	// exec 不会主动关闭 io.PipeWriter；ctx 超时后主进程已被 CommandContext
	// 杀掉，但 pw 仍开着，读循环会永久阻塞。这里监听 ctx，一旦 done 就关闭
	// pw 并 kill 整组，让读循环解除阻塞、cmd.Wait 返回 kill-error。
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
	// §P1-D：字节滑窗累积器——运行中保最近 keep 字节（尾部）、超窗丢中段（保住 shell 错误/退出码所在尾部），
	// 子进程因 pipe 被持续排空不阻塞、不被输出量打断（移除旧 LimitReader+volume-kill），跑到 Wait 返回可信 ExitCode。
	// phase-1 落盘默认关（headSpillBytes=0）。
	accum := newOutputAccum(streamWindow, 0, "", "miniagent_shell_")
	buf := make([]byte, 32*1024)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			_ = accum.write(string(buf[:n])) // 落盘失败 best-effort（phase-1 关，不会触发）
		}
		if rerr != nil {
			break // EOF（pw 关闭）或读错
		}
	}
	_ = accum.closeSink()
	err := <-waitErr
	// 兜底：正常退出后也整组清理一次，防后台 & 残留（不再为 volume kill）。
	killProcessGroup(cmd)
	return accum.finalize(maxOutputChars), err
}

// scrubEnv 复制 env 并移除：所有 MINIAGENT_* 前缀条目，以及变量名（大写后）含
// KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/PASSPHRASE/AUTH/PAT 的条目。后者覆盖 config
// 模式 ${MAIN_API_KEY} 注入的来源变量（非 MINIAGENT_ 前缀但同样承载真实 key），以及
// AWS_ACCESS_KEY_ID、GH_TOKEN/GITHUB_TOKEN/GITHUB_PAT/GITLAB_PAT、DATABASE_PASSWORD、
// MYSQL_PWD、DB_PASS/REDIS_PASS、GPG_PASSPHRASE、BASIC_AUTH/AUTH_HEADER 等高频宿主凭证，
// 降低 LLM 经环境变量 echo 出密钥的概率。PWD/PASS/AUTH 等短关键字会扩大误伤面
// （如 AUTHPROXY、PASSWORDLESS 中含 PASS/PASSWORD）——安全侧倾斜的已知取舍，倾向过度
// 剥离而非泄漏。PAT 单独排除 PATH 族（PATH/PATHEXT/*_PATH），见 hasSecretKeyword 注释。
//
// 已知未覆盖（依赖调用方 OS 隔离兜底，不强制剥离以免误伤 agent 自身 shell 命令
// 所需环境）：DATABASE_URL/SERVICE_URL（URL 过宽）、*_COOKIE、*_DSN、*_CONN。
// 这些是增量泄漏面收窄，不是密钥隔离边界——未列出的凭证名仍继承，且子进程可经
// /proc/$PPID/environ 读到 exec 前的完整环境快照。彻底方案是调用方隔离（容器/独立 UID）；
// key 若经 $MINIAGENT_API_KEY 注入则必在进程 env，procfs 可读。
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

// hasSecretKeyword 报告大写变量名是否含密钥相关关键字。仅服务于 scrubEnv。
// PAT 覆盖 GITHUB_PAT 等 fine-grained token，但 PATH 族变量共享 P-A-T 子串——PATH 是 shell
// 解析可执行路径的必需变量，误剥会让 ls/grep/cat 全部失效。故 PAT 单独走「含 PATH 必为路径类、
// 豁免」分支。COMPAT_*/PATCH_* 等罕见变量会被过度剥离，接受。
func hasSecretKeyword(upperName string) bool {
	for _, kw := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PWD", "PASS", "PASSPHRASE", "AUTH"} {
		if strings.Contains(upperName, kw) {
			return true
		}
	}
	return strings.Contains(upperName, "PAT") && !strings.Contains(upperName, "PATH")
}
