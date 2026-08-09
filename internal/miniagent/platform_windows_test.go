//go:build windows

package miniagent

import (
	"os"
	"path/filepath"
	"testing"
)

// OpenNoFollow on Windows should reject the final component when it is a symlink, but allow regular files.
func TestOpenNoFollow_Windows_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenNoFollow(regular, os.O_RDONLY, 0)
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
	if _, err := OpenNoFollow(link, os.O_RDONLY, 0); err == nil {
		t.Fatal("symlink: expected error, got nil")
	}
}
