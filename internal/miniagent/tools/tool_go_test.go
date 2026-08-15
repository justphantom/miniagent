package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGo_RejectsDestructiveCommand(t *testing.T) {
	got := GoTool(t.TempDir(), 0)
	cases := []string{"run .", "get github.com/x/y", "install golang.org/x/tools",
		"mod tidy", "mod download", "mod init", "generate", "env -w", "bug", "foobar"}
	for _, c := range cases {
		res := got.Call(context.Background(), `{"subcommand":"`+c+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not allowed in default mode") {
			t.Errorf("go %q should be rejected, got: %s", c, res.Output)
		}
	}
}

func TestGo_MissingSubcommand(t *testing.T) {
	res := GoTool(t.TempDir(), 0).Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}

func TestGo_DeniesFileWritingOptions(t *testing.T) {
	got := GoTool(t.TempDir(), 0)
	for _, opt := range []string{"-w", "-write", "-fix", "-modfile=x"} {
		res := got.Call(context.Background(), `{"subcommand":"build","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("build %q should be blocked, got: %s", opt, res.Output)
		}
	}
}

func TestGo_ModuleRootResolution(t *testing.T) {
	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "module")
	os.MkdirAll(moduleDir, 0o755)
	os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0o644)
	res := GoTool(moduleDir, 0).Call(context.Background(), `{"subcommand":"list","args":"-m"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
}

// fmt 是 verify-gate 首步（gofmt -s -l）的修复手段，允许列表成员必须有正向用例锁住
func TestGo_FmtFormatsMalformedFile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if runtime.GOOS == "windows" {
		t.Skip("path/portability handling differs on windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fmttest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.go")
	if err := os.WriteFile(bad, []byte("package p\n\nfunc  f( )  {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := GoTool(dir, 0).Call(context.Background(), `{"subcommand":"fmt"}`)
	if res.IsError {
		t.Fatalf("go fmt should succeed, got: %s", res.Output)
	}
	body, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "package p\n\nfunc f() {}\n" {
		t.Errorf("file not formatted, got %q", string(body))
	}
}

// resolveModuleRoot 找不到 go.mod 时必须返回 startDir（而非字面量 "."），让 go 在 workdir 报错而非跑到进程 cwd
func TestGo_ModuleRootFallbackIsStartDir(t *testing.T) {
	dir := t.TempDir()
	if got := resolveModuleRoot(dir); got != dir {
		t.Errorf("resolveModuleRoot = %q, want %q", got, dir)
	}
	if got := resolveModuleRoot(""); got == "" {
		t.Errorf("resolveModuleRoot(\"\") should fall back to cwd, got empty")
	}
}
