package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// --new* 全族 + 短形 -n（golangci-lint help: `-n, --new`）与 --output.json.path（报告写任意路径）
// 曾漏出 deny 表：--new-from-patch 值是从任意位置读的 PATH，--output.json.path 把报告写到
// workdir 外。名字中的 '.' 参与全等比较，不依赖前缀推断。
func TestLint_RejectsNewFamilyAndOutputPath(t *testing.T) {
	got := LintTool(t.TempDir(), 0, 0)
	for _, arg := range []string{"-n", "--new-from-patch=/x", "--new-from-merge-base=HEAD~1", "--output.json.path=/tmp/probe/lintout.json", "--output.json.path /tmp/probe/lintout.json"} {
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

// 子命令重复形剥离（同 git/go/npm）：{"subcommand":"version","args":"version"} 曾拼成
// `golangci-lint version version`（run 形更糟：`run run ./...` 报 stat ./run 目录不存在）。
func TestLint_SubcommandRepeatedInArgsStripped(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not available")
	}
	res := LintTool(t.TempDir(), 0, 0).Call(context.Background(), `{"subcommand":"version","args":"version"}`)
	if res.IsError {
		t.Errorf("repeated 'version' should be stripped and succeed, got: %s", res.Output)
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

// rtk 的 golangci-lint 包装层在有 findings 时也退出 0（实测 6/6），抹掉 exit_code 契约
// （lint findings 非零退出是正常结果）。原生 exec 后 ExitCode 必须如实透传。
func TestLint_FindingsExitCodePreserved(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ineffassign：赋值后从未使用 —— golangci-lint 默认启用的 linter，稳定产出 1 条 finding。
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package probe\n\nfunc F() {\n\tx := 1\n\tx = 2\n\t_ = x\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := LintTool(dir, 0, 0).Call(context.Background(), `{"subcommand":"run"}`)
	if res.IsError {
		t.Fatalf("findings exit is a normal result, got IsError: %s", res.Output)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 (findings must surface the native exit code; rtk wrapper exited 0)", res.ExitCode)
	}
	if !strings.Contains(res.Output, "ineffassign") {
		t.Errorf("expected the ineffassign finding in output, got: %s", res.Output)
	}
}
