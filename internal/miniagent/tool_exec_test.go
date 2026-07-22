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
	s := ShellTool(t.TempDir(), false, nil)
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
	s := ShellTool(dir, false, nil)
	res := s.Call(context.Background(), `{"command":"pwd"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	cleaned := filepath.Clean(dir)
	if !strings.Contains(res.Output, cleaned) {
		t.Errorf("Output = %q, want contains %q", res.Output, cleaned)
	}
}

func TestShell_BlockedPattern(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"rm -rf /"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "黑名单") {
		t.Errorf("error = %q", res.Output)
	}
}

func TestShell_BlockedPatternCaseInsensitive(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"RM -RF /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestShell_StripsSecretEnv(t *testing.T) {
	t.Setenv("MINIAGENT_API_KEY", "sk-LEAK")
	t.Setenv("DATABASE_URL", "postgres://LEAK")
	t.Setenv("GOPATH", "/test/gopath") // GOPATH 在白名单中
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"env"}`)
	if res.IsError {
		t.Fatalf("shell env failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "sk-LEAK") || strings.Contains(res.Output, "postgres://LEAK") {
		t.Errorf("secret leaked: %q", res.Output)
	}
	if !strings.Contains(res.Output, "GOPATH=/test/gopath") {
		t.Errorf("whitelist env missing: %q", res.Output)
	}
}

func TestShell_NonZeroExitIsError(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"echo out; exit 3"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "退出码") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 超时后整组清理：sh 的孙进程不应残留。
// 跗 short 模式跳过：测试需等 shellTimeout(60s) 触发，耗时过长。
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
	s := ShellTool(t.TempDir(), false, nil)
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

func TestShell_EmptyWorkspaceRoot(t *testing.T) {
	s := ShellTool("", false, nil)
	res := s.Call(context.Background(), `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// 用户传空 JSON 数组必须回落默认黑名单，不能静默放开破坏性命令。
func TestShell_EmptyBlockedPatternsFallsBack(t *testing.T) {
	s := ShellTool(t.TempDir(), false, []string{})
	res := s.Call(context.Background(), `{"command":"rm -rf /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected rm -rf to be blocked even with empty blockedPatterns")
	}
	if !strings.Contains(res.Output, "黑名单") {
		t.Errorf("error = %q", res.Output)
	}
}

// free 模式 + 空 workdir：cmd.Dir="" 由 exec 解释为进程 cwd，不应被
// ensureWorkspaceDir 拒绝（受限模式才需要工作目录约束）。
func TestShell_FreeModeEmptyWorkdir(t *testing.T) {
	s := ShellTool("", true, nil)
	res := s.Call(context.Background(), `{"command":"echo ok-free"}`)
	if res.IsError {
		t.Fatalf("free mode should not require workdir: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok-free") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestShell_BlockedPatternSpacedFlags(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"rm -r -f /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestShell_BypassBase64PipeShRejected(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"echo cm0gLXJmIC8= | base64 -d | sh"}`)
	if !res.IsError {
		t.Fatal("expected base64 pipe-sh to be rejected")
	}
}

func TestShell_MissingWorkspaceRootRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	s := ShellTool(dir, false, nil)
	res := s.Call(context.Background(), `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected missing workspace root to be rejected")
	}
}

