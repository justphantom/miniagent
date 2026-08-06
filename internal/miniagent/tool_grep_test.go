package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeTree 在 dir 下按相对路径写多个文件（自动建父目录），供 grep/glob 测试建树。
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGrepTool_Basic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":     "foo\nbar\n",
		"b.txt":    "foo baz\n",
		"sub/c.go": "bar foo\n",
	})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	for _, want := range []string{"a.go:1:foo", "b.txt:1:foo baz", "sub/c.go:1:bar foo"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("missing %q in:\n%s", want, res.Output)
		}
	}
}

func TestGrepTool_GlobFilter(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": "foo\n", "b.txt": "foo\n"})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo","glob":"*.go"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.go:") || strings.Contains(res.Output, "b.txt:") {
		t.Errorf("glob filter failed: %s", res.Output)
	}
}

func TestGrepTool_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": "foo\n"})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"zzz"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "无命中") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestGrepTool_InvalidPattern(t *testing.T) {
	dir := t.TempDir()
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"["}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestGrepTool_SkipDotGit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":      "foo\n",
		".git/HEAD": "foo\n",
		".git/cfg":  "foo\n",
	})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// 仅命中 a.go；.git 下两处必须跳过。
	if strings.Count(res.Output, ":foo") != 1 {
		t.Errorf(".git not skipped (want 1 hit): %s", res.Output)
	}
}

// P2-9：单文件大小上限。超过 maxGrepFileBytes 的文件入口 Stat 直接跳过，避免
// 无匹配的巨文件逐行扫到 fileOpTimeout(30s) 纯耗 IO。用 Truncate 造稀疏文件
// （Size() 报告大但不占盘），grepFile 在读前就返回，跳过路径不读内容。
func TestGrepFile_SkipsOversized(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 稀疏文件：Size() 报告 maxGrepFileBytes+1，文件系统不实际分配（ext4/tmpfs 支持）。
	if err := f.Truncate(maxGrepFileBytes + 1); err != nil {
		_ = f.Close()
		t.Skipf("truncate to sparse size unsupported: %v", err)
	}
	_ = f.Close()
	re := regexp.MustCompile("foo")
	_, err = grepFile(big, big, re)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("grepFile should skip oversized file with \"too large\" error, got %v", err)
	}
}

// 端到端验证：大文件被跳过、小文件正常命中，结果只含小文件。
func TestGrepTool_SkipsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxGrepFileBytes + 1); err != nil {
		_ = f.Close()
		t.Skipf("truncate unsupported: %v", err)
	}
	_ = f.Close()
	writeTree(t, dir, map[string]string{"small.go": "foo match\n"})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "small.go:1:foo match") {
		t.Errorf("small file hit missing: %s", res.Output)
	}
}
