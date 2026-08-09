package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeTree writes multiple files under dir by relative path (auto-creating parent directories), to build a tree for grep/glob tests.
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
	if !strings.Contains(res.Output, "no matches") {
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
	// Only a.go matches; the two hits under .git must be skipped.
	if strings.Count(res.Output, ":foo") != 1 {
		t.Errorf(".git not skipped (want 1 hit): %s", res.Output)
	}
}

// P2-9: per-file size cap. Files exceeding maxGrepFileBytes are skipped directly at the entry-point Stat, avoiding a
// no-match giant file being scanned line by line until fileOpTimeout(30s) — pure IO waste. Truncate builds a sparse file
// (Size() reports large but no disk is consumed); grepFile returns before reading, and the skipped path is never read.
func TestGrepFile_SkipsOversized(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Sparse file: Size() reports maxGrepFileBytes+1 while the filesystem allocates nothing (supported on ext4/tmpfs).
	if err := f.Truncate(maxGrepFileBytes + 1); err != nil {
		_ = f.Close()
		t.Skipf("truncate to sparse size unsupported: %v", err)
	}
	_ = f.Close()
	re := regexp.MustCompile("foo")
	_, err = grepFile(big, big, re, maxGrepMatches)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("grepFile should skip oversized file with \"too large\" error, got %v", err)
	}
}

// End-to-end verification: the large file is skipped, the small file matches normally, and the result contains only the small file.
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
