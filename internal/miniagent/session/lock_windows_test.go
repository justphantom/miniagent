//go:build windows

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// withSessionLock 在 Windows 上应完成加锁、MkdirAll 0o700、写入并解锁。
func TestWithSessionLock_Windows_WritesAndLocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "s.jsonl")
	if err := withSessionLock(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, func(f *os.File) error {
		_, err := f.WriteString("line\n")
		return err
	}); err != nil {
		t.Fatalf("first lock/write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "line\n" {
		t.Errorf("got %q, want line\\n", data)
	}
	// 连续加锁不应死锁（同进程不互斥，但至少不能报错）。
	if err := withSessionLock(path, os.O_APPEND|os.O_WRONLY, func(f *os.File) error {
		_, err := f.WriteString("line2\n")
		return err
	}); err != nil {
		t.Fatalf("second lock/write: %v", err)
	}
}
