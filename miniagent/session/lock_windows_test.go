//go:build windows

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// withSessionLock on Windows should complete locking, MkdirAll 0o700, writing, and unlocking.
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
	// Locking twice in a row should not deadlock (within the same process it is not mutually exclusive, but at minimum must not error).
	if err := withSessionLock(path, os.O_APPEND|os.O_WRONLY, func(f *os.File) error {
		_, err := f.WriteString("line2\n")
		return err
	}); err != nil {
		t.Fatalf("second lock/write: %v", err)
	}
}
