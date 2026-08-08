package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodemapTool_Basic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":      "",
		"sub/b.go":  "",
		"sub/c.go":  "",
		"deep/d.go": "",
	})
	res := CodemapTool(dir, 0).Call(context.Background(), `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	for _, want := range []string{"a.go", "sub/ (2 items)", "  b.go", "  c.go", "deep/ (1 items)"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("missing %q in:\n%s", want, res.Output)
		}
	}
	// 二级条目缩进两个空格。
	if !strings.Contains(res.Output, "\n  b.go") {
		t.Errorf("expected 2-space indent for nested file:\n%s", res.Output)
	}
}

func TestCodemapTool_DepthLimit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":          "",
		"sub/b.go":      "",
		"sub/deep/c.go": "",
	})
	res := CodemapTool(dir, 0).Call(context.Background(), `{"depth":1}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "sub/ (? items)") {
		t.Errorf("depth boundary dir should be marked (? items):\n%s", res.Output)
	}
	if strings.Contains(res.Output, "b.go") || strings.Contains(res.Output, "deep") {
		t.Errorf("depth=1 should not expand subdirectories:\n%s", res.Output)
	}
	// depth<=0 不限深度。
	res = CodemapTool(dir, 0).Call(context.Background(), `{"depth":-1}`)
	if !strings.Contains(res.Output, "c.go") {
		t.Errorf("depth<=0 should be unlimited:\n%s", res.Output)
	}
}

func TestCodemapTool_EntryLimitTruncates(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for i := range maxCodemapEntries + 10 {
		files[fmt.Sprintf("f%04d.txt", i)] = ""
	}
	writeTree(t, dir, files)
	res := CodemapTool(dir, 0).Call(context.Background(), `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "已停止收集") {
		t.Errorf("expected truncation notice:\n%s", res.Output)
	}
	if n := strings.Count(strings.TrimSpace(res.Output), "\n") + 1; n > maxCodemapEntries+1 {
		t.Errorf("collected %d lines, limit %d", n, maxCodemapEntries)
	}
}

func TestCodemapTool_SkipDotGit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":       "",
		".git/x.go":  "",
		".git/y.txt": "",
	})
	res := CodemapTool(dir, 0).Call(context.Background(), `{}`)
	if strings.Contains(res.Output, ".git") || strings.Contains(res.Output, "x.go") {
		t.Errorf(".git not skipped:\n%s", res.Output)
	}
}

func TestCodemapTool_SkipSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":     "",
		"sub/b.go": "",
	})
	if err := os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "linkdir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "a.go"), filepath.Join(dir, "linkfile.go")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	res := CodemapTool(dir, 0).Call(context.Background(), `{}`)
	if strings.Contains(res.Output, "linkdir") || strings.Contains(res.Output, "linkfile") {
		t.Errorf("symlinks not skipped:\n%s", res.Output)
	}
	// 顶层条目数不应把符号链接计入（a.go + sub = 2）。codemapWalk 不标注根目录，
	// 改由 depth=1 时 sub 的计数间接验证开销过大，此处仅验证符号链接不出现。
}

func TestCodemapTool_MissingRootErrors(t *testing.T) {
	res := CodemapTool(t.TempDir(), 0).Call(context.Background(), `{"path":"/nonexistent/dir"}`)
	if !res.IsError {
		t.Fatal("expected error for missing root")
	}
}

func TestCodemapTool_EmptyDir(t *testing.T) {
	res := CodemapTool(t.TempDir(), 0).Call(context.Background(), `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "空目录") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestCodemapTool_PathArg(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":     "",
		"sub/b.go": "",
	})
	res := CodemapTool(dir, 0).Call(context.Background(), `{"path":"sub"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "b.go") || strings.Contains(res.Output, "a.go") {
		t.Errorf("path=sub should root at subdir:\n%s", res.Output)
	}
}

func TestCodemapTool_InvalidArgs(t *testing.T) {
	res := CodemapTool(t.TempDir(), 0).Call(context.Background(), `{`)
	if !res.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}
