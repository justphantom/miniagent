package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestLint_RejectsDisallowedSubcommand(t *testing.T) {
	got := LintTool(t.TempDir(), 0, 0)
	for _, sub := range []string{"cache", "config", "completion", "foobar"} {
		res := got.Call(context.Background(), `{"subcommand":"`+sub+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not allowed") {
			t.Errorf("golangci-lint %q should be rejected, got: %s", sub, res.Output)
		}
	}
}

func TestLint_MissingSubcommand(t *testing.T) {
	res := LintTool(t.TempDir(), 0, 0).Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}

func TestLint_RejectsFixArg(t *testing.T) {
	got := LintTool(t.TempDir(), 0, 0)
	// -w 不在列表：golangci-lint 无 -w 改写语义，旧前缀 "-w" 属误杀已删。
	for _, arg := range []string{"--fix", "-fix", "-f", "--write", "--enable-all", "--new", "--new-from-rev=HEAD"} {
		res := got.Call(context.Background(), `{"subcommand":"run","args":"`+arg+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("golangci-lint run with %q should be blocked, got: %s", arg, res.Output)
		}
	}
}

func TestLint_AllowedSubcommandRuns(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not available")
	}
	dir := t.TempDir()
	res := LintTool(dir, 0, 0).Call(context.Background(), `{"subcommand":"version"}`)
	if res.IsError {
		t.Fatalf("golangci-lint version should succeed, got: %s", res.Output)
	}
}

// 二进制不存在时必须返回 IsError + no such file 消息（不 panic、不崩溃），
// 与 git/go/npm 经 runLimitedOutput 的既有行为对齐。用空 PATH 子目录强制 LookPath 失败，
// 不依赖运行环境是否安装了 golangci-lint。
func TestLint_BinaryMissingReturnsError(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-empty-dir")
	res := LintTool(t.TempDir(), 0, 0).Call(context.Background(), `{"subcommand":"version"}`)
	if !res.IsError {
		t.Errorf("expected IsError when binary is missing, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "executable file not found in $PATH") && !strings.Contains(res.Output, "no such file or directory") {
		t.Errorf("expected not-found message, got: %s", res.Output)
	}
}

// SplitTruncate：lint 结果按文件逐条输出，截断时保 head+tail。
func TestLint_SplitTruncateSet(t *testing.T) {
	if !LintTool(t.TempDir(), 0, 0).SplitTruncate {
		t.Fatal("golangci-lint tool must set SplitTruncate (per-file findings span head to tail)")
	}
}
