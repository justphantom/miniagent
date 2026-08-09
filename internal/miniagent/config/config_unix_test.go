//go:build unix

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ReadFileLimited rejects final-component symlinks via OpenNoFollow (P2 fix), preventing the config (which contains the API key)
// from being symlink-hijacked. Regression review blind spot: the security gain of OpenNoFollow hardening ReadFileLimited had no test coverage before.
func TestReadFileLimited_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "realPath.json")
	if err := os.WriteFile(realPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileLimited(link, 1024); err == nil {
		t.Error("ReadFileLimited should reject symlink (OpenNoFollow prevents config hijacking)")
	}
	// Control: a real file (non-symlink) reads normally.
	if _, err := ReadFileLimited(realPath, 1024); err != nil {
		t.Errorf("real file should read normally: %v", err)
	}
}
