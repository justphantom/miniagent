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
	s := ShellTool(t.TempDir())
	res := s.Call(context.Background(), `{"command":"echo hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestShell_CwdIsWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	s := ShellTool(dir)
	res := s.Call(context.Background(), `{"command":"pwd"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	cleaned := filepath.Clean(dir)
	if !strings.Contains(res.Output, cleaned) {
		t.Errorf("Output = %q, want contains %q", res.Output, cleaned)
	}
}

func TestShell_NonZeroExitIsError(t *testing.T) {
	s := ShellTool(t.TempDir())
	res := s.Call(context.Background(), `{"command":"echo out; exit 3"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "退出码") {
		t.Errorf("Output = %q", res.Output)
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
	marker := "miniagent_uniq_sleep_marker_9f3k2"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "pkill", "-9", "-f", marker).Run()
	})
	s := ShellTool(t.TempDir())
	// exec -a 让 sleep 进程名带 marker，pgrep -f 才能精确匹配。
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"exec -a `+marker+` sleep 600"}`)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Error("expected timeout error")
	}
	if elapsed > 75*time.Second {
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
	s := ShellTool("")
	res := s.Call(context.Background(), `{"command":"echo ok-empty"}`)
	if res.IsError {
		t.Fatalf("empty workdir should not fail: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok-empty") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 子进程继承父进程全量环境（不再有白名单）。
func TestShell_InheritsFullEnv(t *testing.T) {
	t.Setenv("MINIAGENT_TEST_VAR", "inherited")
	s := ShellTool(t.TempDir())
	res := s.Call(context.Background(), `{"command":"echo $MINIAGENT_TEST_VAR"}`)
	if res.IsError {
		t.Fatalf("shell failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "inherited") {
		t.Errorf("env not inherited: %q", res.Output)
	}
}

// 空命令：参数校验失败。
func TestShell_EmptyCommandRejected(t *testing.T) {
	s := ShellTool(t.TempDir())
	res := s.Call(context.Background(), `{"command":"   "}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}
