package miniagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write 对 FIFO 必须在 Lstat 阶段拒绝（IsRegular 校验），而非走到 Rename 才
// 报含糊错误；与 edit/multi_edit 对齐（审查 P3-7）。
func TestWriteFile_RejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"fifo","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected FIFO to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
}

// write 对目录目标必须明确拒绝，而非 Rename 时报 EISDIR（审查 P3-7）。
func TestWriteFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"sub","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected directory target to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 字符设备（/dev/null）同样应被 IsRegular 校验拒绝（审查 P3-7）。
func TestWriteFile_RejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	dir := t.TempDir()
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"/dev/null","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected device file to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
}

// IsRegular 校验不应误伤已存在的普通文件覆盖写入。
func TestWriteFile_OverwritesRegularFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"old.txt","content":"new content"}`)
	if res.IsError {
		t.Fatalf("overwrite regular file failed: %s", res.Output)
	}
	got, _ := os.ReadFile(existing)
	if string(got) != "new content" {
		t.Errorf("content = %q, want %q", got, "new content")
	}
}
