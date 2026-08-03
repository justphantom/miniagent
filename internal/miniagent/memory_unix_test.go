//go:build !windows

package miniagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// memory.jsonl 为非普通文件（如 FIFO）时须拒绝。
func TestMemory_NonRegularFileRejected(t *testing.T) {
	dir := t.TempDir()
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o750); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(maDir, "memory.jsonl")
	if err := os.WriteFile(fifo, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 先写一次正常文件确保流程通，再换成 FIFO
	_ = os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	if r := readMemoryTool(dir); !r.IsError {
		t.Errorf("read should reject FIFO memory file")
	}
	if r := writeMemoryTool(dir, "x"); !r.IsError {
		t.Errorf("write should reject FIFO memory file")
	}
}
