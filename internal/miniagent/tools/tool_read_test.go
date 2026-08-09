package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadFile_RelativePath(t *testing.T) {
	dir := writeTemp(t, "a.txt", "hello world")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"a.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// Output carries line numbers by default (edit relies on line numbers to locate).
	if !strings.Contains(res.Output, "1 │ hello world") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_AbsoluteInsideRoot(t *testing.T) {
	dir := writeTemp(t, "b.txt", "abs ok")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"`+filepath.Join(dir, "b.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 │ abs ok") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_Range(t *testing.T) {
	dir := writeTemp(t, "r.txt", "line1\nline2\nline3\n")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"r.txt","offset":2,"limit":1}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "2 │ line2") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_Truncates(t *testing.T) {
	long := strings.Repeat("a", maxReadFileBytes/4+500)
	dir := writeTemp(t, "big.txt", long)
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"big.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "…") {
		t.Errorf("missing truncation marker")
	}
}

// P2-3: read uses Lstat to decide the file type and directly rejects a symlink as the final path component (aligned with
// edit/openNoFollow). The old Stat would follow the symlink to read the target, contradicting the "reject symlinks" description.
// Intermediate directory symlinks are still followed (read has no path constraints; only the final component is guarded by Lstat/O_NOFOLLOW).
func TestReadFile_SymlinkFinalComponentRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "real.txt")
	if err := os.WriteFile(target, []byte("via symlink"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"link"}`)
	if !res.IsError {
		t.Fatal("expected symlink final component to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
	// The target content should not be read out (prevents a symlink read from escaping outside workdir).
	if strings.Contains(res.Output, "via symlink") {
		t.Errorf("symlink target content leaked: %q", res.Output)
	}
}

// Empty workdir: read with a relative path still works (relies on the caller process's cwd).
func TestReadFile_EmptyWorkdir(t *testing.T) {
	dir := writeTemp(t, "cwd.txt", "from-cwd")
	res := ReadFileTool("", 0, 0).Call(context.Background(), `{"path":"`+filepath.Join(dir, "cwd.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 │ from-cwd") {
		t.Errorf("Output = %q", res.Output)
	}
}

// offset exceeding the file's line count should be returned as IsError (rather than silently empty output).
func TestReadFile_OffsetOutOfBoundsIsError(t *testing.T) {
	dir := writeTemp(t, "small.txt", "only one line")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"small.txt","offset":42}`)
	if !res.IsError {
		t.Fatal("expected error for offset out of bounds")
	}
	if !strings.Contains(res.Output, "offset 42") {
		t.Errorf("Output = %q", res.Output)
	}
}

// An empty file should not emit a spurious empty line "1 │ "; it returns an empty string directly.
func TestReadFile_EmptyFileReturnsBlank(t *testing.T) {
	dir := writeTemp(t, "empty.txt", "")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"empty.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "" {
		t.Errorf("Output = %q, want empty", res.Output)
	}
}

// Non-regular files (FIFO/device/socket) must be rejected, otherwise a FIFO would block open indefinitely.
func TestReadFile_RejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"fifo"}`)
	if !res.IsError {
		t.Fatal("expected FIFO to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
}

// /dev/null is a character device and should be rejected (rather than returning empty content).
func TestReadFile_RejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	res := ReadFileTool("", 0, 0).Call(context.Background(), `{"path":"/dev/null"}`)
	if !res.IsError {
		t.Fatal("expected device file to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
}

// Binary content (containing NUL) must be rejected to avoid garbled output polluting the LLM context.
func TestReadFile_RejectsBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(bin, []byte("ABC\x00\x01DEF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"bin.dat"}`)
	if !res.IsError {
		t.Fatal("expected binary to be rejected")
	}
	if !strings.Contains(res.Output, "binary") {
		t.Errorf("Output = %q", res.Output)
	}
}

// Pure text whose NUL appears outside the 8 KiB scan window (>8192 bytes) should be read normally.
func TestReadFile_TextWithLateNULPasses(t *testing.T) {
	dir := t.TempDir()
	// 9000 bytes of pure text + a trailing NUL: the scan window only looks at the first 8192, so it should pass.
	content := strings.Repeat("a", 9000) + "\x00"
	bin := filepath.Join(dir, "late.txt")
	if err := os.WriteFile(bin, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"late.txt"}`)
	if res.IsError {
		t.Fatalf("late-NUL text should pass: %s", res.Output)
	}
}
