package miniagent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShell_RunsCommand(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestShell_CwdIsWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	s := ShellTool(dir, 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"pwd"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	cleaned := filepath.Clean(dir)
	if !strings.Contains(res.Output, cleaned) {
		t.Errorf("Output = %q, want contains %q", res.Output, cleaned)
	}
}

// 非 0 退出是命令的合法结果：IsError=false、ExitCode=命令退出码、stdout 完整保留。
// 旧版把非 0 退出当 IsError=true 并把退出码拼进 Output 文本，已改为结构化 ExitCode。
func TestShell_NonZeroExitReturnsExitCode(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo out; exit 3"}`)
	if res.IsError {
		t.Fatalf("non-zero exit should not be IsError: %s", res.Output)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Output, "out") {
		t.Errorf("stdout lost: Output = %q", res.Output)
	}
}

// 超时后整组清理：sh 的孙进程不应残留。
// short 模式跳过：测试需等 shellTimeout(60s) 触发，耗时过长。
func TestShell_KillsGrandchildOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires shellTimeout to elapse")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available (exec -a needs bash)")
	}
	marker := "miniagent_uniq_sleep_marker_9f3k2"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "pkill", "-9", "-f", marker).Run()
	})
	s := ShellTool(t.TempDir(), 2*time.Second, ModeAuto, 0, 0)
	// bash -c 'exec -a marker sleep 600'：让 sleep 进程名带 marker 供 pgrep -f 精确匹配。
	// 用 2s 超时（非默认 60s）加速；仍验证进程组被 kill（孙进程清理）。
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"bash -c 'exec -a `+marker+` sleep 600'"}`)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Error("expected timeout error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
	time.Sleep(time.Second)
	pgrepCtx, pgrepCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pgrepCancel()
	out, err := exec.CommandContext(pgrepCtx, "pgrep", "-f", marker).Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("grandchild still alive after kill: %s", out)
	}
}

// workdir 为空：cmd.Dir 留空，exec 继承父进程 cwd。
func TestShell_EmptyWorkdirInheritsCwd(t *testing.T) {
	s := ShellTool("", 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo ok-empty"}`)
	if res.IsError {
		t.Fatalf("empty workdir should not fail: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok-empty") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 子进程继承父进程全量环境（MINIAGENT_* 前缀除外），其他变量原样透传。
func TestShell_InheritsFullEnv(t *testing.T) {
	t.Setenv("MINIAGENT_TEST_INHERIT", "inherited")
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo $MINIAGENT_TEST_INHERIT"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "inherited") {
		t.Errorf("MINIAGENT_* should be scrubbed: %q", res.Output)
	}
	// 非 MINIAGENT_ 前缀变量应正常继承。
	t.Setenv("TEST_SHELL_INHERIT", "passed-through")
	res2 := s.Call(context.Background(), `{"command":"echo $TEST_SHELL_INHERIT"}`)
	if res2.IsError {
		t.Fatalf("shell failed: %s", res2.Output)
	}
	if !strings.Contains(res2.Output, "passed-through") {
		t.Errorf("non-MINIAGENT env not inherited: %q", res2.Output)
	}
}

// MINIAGENT_API_KEY 必须被剥离，避免 LLM 通过 shell 读取宿主密钥。
func TestShell_ScrubsAPIKey(t *testing.T) {
	t.Setenv("MINIAGENT_API_KEY", "sk-secret-leak")
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo [$MINIAGENT_API_KEY]"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "sk-secret-leak") {
		t.Errorf("API key leaked to child: %q", res.Output)
	}
}

// 所有 MINIAGENT_* 前缀变量都应被剥离（含 BASE_URL 等配置信息）。
func TestShell_ScrubsAllMiniagentVars(t *testing.T) {
	t.Setenv("MINIAGENT_API_KEY", "sk-leak")
	t.Setenv("MINIAGENT_BASE_URL", "https://private.example.internal")
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"env | grep MINIAGENT_ | wc -l"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if !strings.Contains(strings.TrimSpace(res.Output), "0") {
		t.Errorf("MINIAGENT_* vars leaked: %q", res.Output)
	}
}

// 空命令：参数校验失败。
func TestShell_EmptyCommandRejected(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"   "}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// 自定义超时：sleep 5 在 200ms 后被杀，返回 IsError 且含「超时」，1s 内返回。
func TestShell_CustomTimeout(t *testing.T) {
	s := ShellTool(t.TempDir(), 200*time.Millisecond, ModeAuto, 0, 0)
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"sleep 5"}`)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Fatal("expected timeout error")
	}
	if res.ExitCode != exitCodeNotSet {
		t.Errorf("ExitCode = %d, want %d (timeout)", res.ExitCode, exitCodeNotSet)
	}
	if !strings.Contains(res.Output, "超时") {
		t.Errorf("Output = %q", res.Output)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
}

// timeout=0 用默认路径（不实际等 60s，只验正常执行）。
func TestShell_ZeroTimeoutUsesDefault(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo ok"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Errorf("Output = %q", res.Output)
	}
}

// default 模式拒绝 sudo/su（词边界，覆盖 "cd /x && sudo ..."）。
func TestShellTool_DefaultRejectsSudo(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeDefault, 0, 0)
	for _, cmd := range []string{"sudo rm -f /", "su root", "cd /tmp && sudo ls"} {
		res := s.Call(context.Background(), `{"command":"`+cmd+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "sudo") {
			t.Errorf("default mode should reject %q: %s", cmd, res.Output)
		}
	}
}

// auto 模式放行 sudo/su；cd 不拦（不拦 cd 出 workdir，审查 v2 #10）。
func TestShellTool_AutoAllowsAndCdNotBlocked(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	// sudo 不在 default → 不拦截（命令本身因环境可能失败，但不被前置拒绝）。
	res := s.Call(context.Background(), `{"command":"echo has sudo in text"}`)
	if res.IsError {
		t.Errorf("auto mode should not pre-filter: %s", res.Output)
	}
	// "sudo" 作为 echo 参数（词边界匹配仍命中），但 auto 模式不拒。
	if strings.Contains(res.Output, "default 模式") {
		t.Errorf("auto should not emit default-mode rejection: %s", res.Output)
	}
}

// 父 ctx 超时（max-duration / 信号）不应被误报为 shell 自身超时（修复 R5）：
// shell timeout 设长（不触发），父 ctx 短超时令 sleep 被取消 → 输出须不含「命令超时」文案。
func TestShell_ParentCancelNotReportedAsShellTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires parent ctx to elapse")
	}
	parent, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	s := ShellTool(t.TempDir(), 60*time.Second, ModeAuto, 0, 0)
	res := s.Call(parent, `{"command":"sleep 3"}`)
	if strings.Contains(res.Output, "命令超时") {
		t.Errorf("parent cancel misreported as shell timeout: %s", res.Output)
	}
}

// default 模式拦截常见特权提升器（P2-12 + 三轮 P3）：sudo/su/doas/pkexec/gsudo/run0，
// 以及专有特权/命名空间工具 setpriv/nsenter/unshare/chroot/machinectl（低频专有，误伤小）。
// please 不在列：英文常用词，误伤合法命令（二次审查回归）。
// auto 模式不拦，此处仅验 default 模式前置拒绝（不实际执行，无环境依赖）。
func TestShellTool_DefaultRejectsPrivilegeEscalators(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeDefault, 0, 0)
	for _, cmd := range []string{
		"sudo rm -f /",
		"su root",
		"doas sh",
		"pkexec ls /",
		"gsudo whoami",
		"run0 id",
		"setpriv --reuid 0 sh",
		"nsenter -t 1 -u sh",
		"unshare -r sh",
		"chroot / sh",
		"machinectl shell",
		// 中段命令（cd /tmp && <escalator>）也必须拦——词边界正则覆盖复合命令。
		"cd /tmp && doas touch x",
		"cd /tmp && setpriv ls",
		"cd /var && unshare -r cat /etc/shadow",
	} {
		res := s.Call(context.Background(), `{"command":"`+cmd+`"}`)
		if !res.IsError {
			t.Errorf("default mode should reject %q: %s", cmd, res.Output)
		}
	}
}

// default 模式不得误伤含 please 的合法命令（二次审查 P2 #1：please 是英文常用词）。
func TestShellTool_PleaseNotFalselyRejected(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeDefault, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo please review"}`)
	if res.IsError && strings.Contains(res.Output, "禁止特权提升器") {
		t.Fatalf("please wrongly rejected: %s", res.Output)
	}
}

// §P1-D：超 100k 字符的 shell 输出保尾部（含退出码所在末尾）、加 banner、ExitCode 可信（命令跑完）。
func TestShell_HighOutputKeepsTail(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"seq 1 400000"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0（命令应跑完，退出码可信）", res.ExitCode)
	}
	if !strings.Contains(res.Output, "仅保留尾部") {
		t.Errorf("超量输出应含 banner: len=%d", len(res.Output))
	}
	if !strings.Contains(res.Output, "399999") || !strings.Contains(res.Output, "400000") {
		t.Errorf("应保留尾部 399999/400000: len=%d", len(res.Output))
	}
	if strings.Contains(res.Output, "1\n2\n3\n") {
		t.Errorf("不应保留首部 1\\n2\\n3\\n（中段应被丢）")
	}
}

// §P1-D 回归：<100k 字符命令字节级等价（无 banner、首尾完整）。
func TestShell_SmallOutputNoBanner(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"seq 1 100"}`)
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if strings.Contains(res.Output, "仅保留尾部") {
		t.Errorf("小输出不应有 banner")
	}
	if !strings.Contains(res.Output, "1\n") || !strings.Contains(res.Output, "\n100") {
		t.Errorf("小输出应完整含首尾: %q", res.Output)
	}
}

// §P1-D：移除 volume-kill 后 ctx 超时语义仍生效（sleep > timeout → IsError + exitCodeNotSet + 超时提示）。
func TestShell_TimeoutStillReported(t *testing.T) {
	s := ShellTool(t.TempDir(), 100*time.Millisecond, ModeAuto, 0, 0)
	res := s.Call(context.Background(), `{"command":"sleep 5"}`)
	if !res.IsError {
		t.Errorf("超时应 IsError=true")
	}
	if res.ExitCode != exitCodeNotSet {
		t.Errorf("超时 ExitCode = %d, want exitCodeNotSet", res.ExitCode)
	}
	if !strings.Contains(res.Output, "超时") {
		t.Errorf("超时应含提示: %q", res.Output)
	}
}
