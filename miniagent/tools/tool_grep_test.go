package tools

import (
	"bytes"
	"context"
	"fmt"
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

// A missing/inaccessible search root must surface as the real error, not "no matches": a typo'd path
// is otherwise indistinguishable from an empty result and the agent stops searching (glob already
// propagates the root error; grep must not diverge).
// O5: a malformed glob must surface as an error, not a misleading "no matches".
func TestGrepTool_InvalidGlob(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": "foo\n"})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo","glob":"[a-"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "invalid glob") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestGrepTool_MissingRootErrors(t *testing.T) {
	dir := t.TempDir()
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo","path":"nope"}`)
	if !res.IsError {
		t.Fatalf("expected error for missing root, got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "search") || !strings.Contains(res.Output, "failed") {
		t.Errorf("error should report the failed search: %q", res.Output)
	}
	if strings.Contains(res.Output, "no matches") {
		t.Errorf("missing root must not be reported as no matches: %q", res.Output)
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

// Exactly maxMatches matches plus later non-matching files must NOT report truncation: truncation is
// only proven by a match found beyond the cap. The old pre-file SkipAll flagged any walkable file.
func TestGrepTool_ExactMatchesNotTruncated(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for i := range maxGrepMatches {
		files[fmt.Sprintf("f%03d.txt", i)] = "foo\n"
	}
	files["zz_empty.txt"] = ""
	writeTree(t, dir, files)
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if strings.Count(res.Output, ":foo") != maxGrepMatches {
		t.Errorf("want exactly %d matches, got %d", maxGrepMatches, strings.Count(res.Output, ":foo"))
	}
	if strings.Contains(res.Output, "collection stopped") {
		t.Errorf("complete result must not claim truncation: %s", tailOf(res.Output))
	}
}

// One match beyond the cap proves unseen matches exist and must report truncation.
func TestGrepTool_TruncationBeyondCap(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for i := range maxGrepMatches + 1 {
		files[fmt.Sprintf("f%03d.txt", i)] = "foo\n"
	}
	writeTree(t, dir, files)
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if strings.Count(res.Output, ":foo") != maxGrepMatches {
		t.Errorf("want exactly %d matches, got %d", maxGrepMatches, strings.Count(res.Output, ":foo"))
	}
	if !strings.Contains(res.Output, "collection stopped") {
		t.Errorf("a match beyond the cap must report truncation, tail: %s", tailOf(res.Output))
	}
}

func tailOf(s string) string {
	if len(s) > 200 {
		return s[len(s)-200:]
	}
	return s
}

// The binary sniff must cover the full advertised 8192-byte window: bufio's default 4096 buffer caps
// Peek, so a NUL at offset 5000 used to pass and the "binary" line was searched and emitted.
func TestGrepTool_BinaryNulPast4096Skipped(t *testing.T) {
	dir := t.TempDir()
	buf := bytes.Repeat([]byte("A"), 9000)
	buf[5000] = 0
	if err := os.WriteFile(filepath.Join(dir, "late.bin"), buf, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"plain.go": "foo\n"})
	res := GrepTool(dir, 0, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if strings.Contains(res.Output, "late.bin") {
		t.Errorf("file with NUL at offset 5000 must be treated as binary and skipped, got: %s", tailOf(res.Output))
	}
	if !strings.Contains(res.Output, "plain.go:1:foo") {
		t.Errorf("plain file hit missing: %s", res.Output)
	}
}
