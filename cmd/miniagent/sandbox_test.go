package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// 相对路径落在 workdir 子树内，应通过。
func TestCheckConfine_RelativeInside(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, "sub/file.txt"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// 绝对路径落在 workdir 子树内，应通过。
func TestCheckConfine_AbsoluteInside(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, filepath.Join(root, "sub", "file.txt")); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// 路径越出 workdir 须拒绝。
func TestCheckConfine_OutsideRejected(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := checkConfine(root, outside); err == nil {
		t.Error("expected outside path to be rejected")
	}
}

// path="." 或 workdir 本身须拒绝，防止覆盖整个 workdir。
func TestCheckConfine_RootItselfRejected(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, "."); err == nil {
		t.Error("expected workdir itself to be rejected")
	}
	if err := checkConfine(root, root); err == nil {
		t.Error("expected workdir itself to be rejected")
	}
}

// 已存在路径分量含符号链接时须拒绝。
func TestCheckConfine_SymlinkComponentRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := checkConfine(root, "linkdir/file.txt"); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Errorf("expected symlink component to be rejected, got: %v", err)
	}
}

// 不存在子目录允许通过（后续由工具创建）。
func TestCheckConfine_NonExistentSubdirAllowed(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, "not-yet/exists.txt"); err != nil {
		t.Errorf("non-existent subdir should be allowed: %v", err)
	}
}

// confineWrap 包装的 write 工具在路径含符号链接时拒绝，不执行原工具。
func TestConfineWrap_BlocksSymlinkPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkdir := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, linkdir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	tool := miniagent.WriteFileTool(root, 0)
	wrapped := confineWrap(tool, root)
	r := wrapped.Call(context.Background(), `{"path":"linkdir/pwned.txt","content":"x"}`)
	if !r.IsError || !strings.Contains(r.Output, "符号链接") {
		t.Errorf("expected confineWrap to reject symlink path, got: %+v", r)
	}
	// 目标文件不应被创建/覆盖。
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Error("symlink target was written outside workdir")
	}
}

// confineWrap 正常路径仍允许写入。
func TestConfineWrap_AllowsRegularPath(t *testing.T) {
	root := t.TempDir()
	tool := miniagent.WriteFileTool(root, 0)
	wrapped := confineWrap(tool, root)
	r := wrapped.Call(context.Background(), `{"path":"sub/ok.txt","content":"hello"}`)
	if r.IsError {
		t.Fatalf("expected regular path to pass: %s", r.Output)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub", "ok.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("file not written correctly: %q err=%v", got, err)
	}
}

// checkConfine：.. 越界拒；已存在路径分量含符号链接亦拒（缩小 TOCTOU 窗口）。
func TestCheckConfine(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.txt")
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkConfine(dir, "inner.txt"); err != nil {
		t.Errorf("inner relative rejected: %v", err)
	}
	if err := checkConfine(dir, filepath.Join(dir, "..", "outside")); err == nil {
		t.Error("escape via .. should be rejected")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// 已存在分量是符号链接时拒绝，防止 IO 落到 workdir 外（default 模式仍是薄软约束，
	// 真隔离仍靠 OS；但不再主动放行明显的软链越界）。
	if err := checkConfine(dir, "link"); err == nil {
		t.Error("existing symlink component should be rejected")
	}
}

// checkConfine 拒绝 path="." 或等于 workdir 绝对路径：rename 覆盖目录会 EISDIR
// （错误含糊），且 MkdirAll/Rename 真生效将摧毁整个 workdir（审查 P3-8）。
func TestCheckConfine_RejectsWorkdirRoot(t *testing.T) {
	dir := t.TempDir()
	if err := checkConfine(dir, "."); err == nil {
		t.Error(`path="." should be rejected as workdir root`)
	}
	if err := checkConfine(dir, dir); err == nil {
		t.Error("absolute path equal to workdir root should be rejected")
	}
}
