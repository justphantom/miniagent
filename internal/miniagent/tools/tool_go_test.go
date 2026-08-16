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
	got := GoTool(t.TempDir(), 0, 0)
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
	res := GoTool(t.TempDir(), 0, 0).Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}

func TestGo_DeniesFileWritingOptions(t *testing.T) {
	got := GoTool(t.TempDir(), 0, 0)
	// -w 写 go env；-fix 改源码；-modfile 换任意 go.mod；-toolexec/-vettool/-exec 执行外部程序；
	// -C chdir 越过 workdir 落点。exec/-vettool 各自属于 test/vet 子命令的执行通道。
	for _, opt := range []string{"-w", "--write", "-fix", "-modfile=x", "-toolexec=/tmp/tool", "-C /tmp"} {
		res := got.Call(context.Background(), `{"subcommand":"build","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("build %q should be blocked, got: %s", opt, res.Output)
		}
	}
	for _, c := range []struct{ sub, opt string }{{"test", "-exec=/tmp/wrap"}, {"vet", "-vettool=/tmp/vet"}} {
		res := got.Call(context.Background(), `{"subcommand":"`+c.sub+`","args":"`+c.opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("%s %q should be blocked, got: %s", c.sub, c.opt, res.Output)
		}
	}
	// -o 树外写拒、树内相对放行（AGENTS：二进制放 bin/）。
	for _, opt := range []string{"-o=/tmp/evil", "-o /tmp/evil", "-o ../evil", "-coverprofile=/tmp/c.out", "-memprofile=/tmp/m.out", "-trace=/tmp/t.out"} {
		res := got.Call(context.Background(), `{"subcommand":"build","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "outside the workdir") {
			t.Errorf("build %q should be blocked as out-of-tree write, got: %s", opt, res.Output)
		}
	}
	res := got.Call(context.Background(), `{"subcommand":"build","args":"-o bin/app ."}`)
	if res.IsError && strings.Contains(res.Output, "outside the workdir") {
		t.Errorf("in-tree -o should not be blocked, got: %s", res.Output)
	}
}

// clean 的全局缓存类旗标（含 -fuzzcache/-i 这两个旧前缀漏掉的）按子命令拦截。
func TestGo_DeniesCleanCacheOptions(t *testing.T) {
	got := GoTool(t.TempDir(), 0, 0)
	for _, opt := range []string{"-cache", "-modcache", "-testcache", "-fuzzcache", "-i"} {
		res := got.Call(context.Background(), `{"subcommand":"clean","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("clean %q should be blocked, got: %s", opt, res.Output)
		}
	}
}

// 旧前缀误杀回归：-work/-overlay 只读或源码级选项、-cache=off（test）不因 "-w"/"-o"/"-cache" 前缀被拒。
func TestGo_DoesNotOverblockLegitFlags(t *testing.T) {
	got := GoTool(t.TempDir(), 0, 0)
	for _, c := range []struct{ sub, opt string }{{"test", "-work"}, {"build", "-overlay=f.json"}, {"test", "-cache=off"}} {
		res := got.Call(context.Background(), `{"subcommand":"`+c.sub+`","args":"`+c.opt+`"}`)
		if res.IsError && strings.Contains(res.Output, "blocked") {
			t.Errorf("%s %q should not be blocked by prefix overreach, got: %s", c.sub, c.opt, res.Output)
		}
	}
}

// SplitTruncate：go test/build 的 FAIL 明细在输出尾部，head 截断只剩包列表。
func TestGo_SplitTruncateSet(t *testing.T) {
	if !GoTool(t.TempDir(), 0, 0).SplitTruncate {
		t.Fatal("go tool must set SplitTruncate (FAIL details live in the tail)")
	}
}

// resolveModuleRoot must not climb above startDir: a parent-module go.mod would place go/npm's cwd
// (and module-level writes: go.mod/go.sum) outside the workdir subtree (default-mode escape).
func TestGo_ModuleRootNeverEscapesStartDir(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "workdir")
	os.MkdirAll(inner, 0o750)
	os.WriteFile(filepath.Join(base, "go.mod"), []byte("module parent\n"), 0o600) // parent has go.mod, inner does not
	if got := resolveModuleRoot(inner); got != inner {
		t.Errorf("resolveModuleRoot climbed to %q, want %q (no upward escape)", got, inner)
	}
	// go.mod inside startDir is still found.
	os.WriteFile(filepath.Join(inner, "go.mod"), []byte("module inner\n"), 0o600)
	if got := resolveModuleRoot(inner); got != inner {
		t.Errorf("resolveModuleRoot = %q, want %q", got, inner)
	}
}

func TestGo_ModuleRootResolution(t *testing.T) {
	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "module")
	os.MkdirAll(moduleDir, 0o755)
	os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0o644)
	res := GoTool(moduleDir, 0, 0).Call(context.Background(), `{"subcommand":"list","args":"-m"}`)
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
	res := GoTool(dir, 0, 0).Call(context.Background(), `{"subcommand":"fmt"}`)
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
