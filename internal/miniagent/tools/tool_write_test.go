package tools

import (
	"context"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A field-name typo (contents vs content) or an omitted content must be a loud error, not a silent
// truncate-to-zero reported as success: decodeStrict rejects the unknown key, and empty content is
// refused outright (empty-file creation is not expressible via write).
func TestWriteFile_RejectsTypoAndEmptyContent(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(existing, []byte("real body"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"old.txt","contents":"new body"}`)
	if !res.IsError {
		t.Fatal("typo'd field name should be rejected by strict decoding")
	}
	res = WriteFileTool(dir, 0).Call(context.Background(), `{"path":"old.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "missing parameter: content") {
		t.Errorf("omitted content should error, got: %s", res.Output)
	}
	res = WriteFileTool(dir, 0).Call(context.Background(), `{"path":"old.txt","content":""}`)
	if !res.IsError || !strings.Contains(res.Output, "missing parameter: content") {
		t.Errorf("empty content should error, got: %s", res.Output)
	}
	// the existing file must survive all three attempts
	got, _ := os.ReadFile(existing)
	if string(got) != "real body" {
		t.Errorf("existing file was clobbered: %q", got)
	}
	if res.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("ExitCode = %d, want ExitCodeNotSet (no command ran)", res.ExitCode)
	}
}

// write must reject a FIFO at the Lstat stage (IsRegular check), rather than reporting an ambiguous
// error at Rename; aligned with edit (review P3-7).
func TestWriteFile_RejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"fifo","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected FIFO to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
}

// write must explicitly reject a directory target, rather than reporting EISDIR at Rename (review P3-7).
func TestWriteFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"sub","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected directory target to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
}

// A character device (/dev/null) should likewise be rejected by the IsRegular check (review P3-7).
func TestWriteFile_RejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	dir := t.TempDir()
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"/dev/null","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected device file to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
}

// The IsRegular check should not falsely reject an overwrite of an existing regular file.
func TestWriteFile_OverwritesRegularFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"old.txt","content":"new content"}`)
	if res.IsError {
		t.Fatalf("overwrite regular file failed: %s", res.Output)
	}
	got, _ := os.ReadFile(existing)
	if string(got) != "new content" {
		t.Errorf("content = %q, want %q", got, "new content")
	}
}
