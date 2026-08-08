//go:build windows

package miniagent

import (
	"os"
	"path/filepath"
	"testing"
)

// openNoFollow 在 Windows 上应拒绝最终分量为符号链接，但允许普通文件。
func TestOpenNoFollow_Windows_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openNoFollow(regular, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("regular file: unexpected error: %v", err)
	}
	_ = f.Close()

	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := openNoFollow(link, os.O_RDONLY, 0); err == nil {
		t.Fatal("symlink: expected error, got nil")
	}
}

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
