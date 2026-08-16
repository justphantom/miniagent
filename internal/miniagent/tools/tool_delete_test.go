package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDelete_File(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600)
	res := DeleteTool(dir, 0).Call(context.Background(), `{"path":"f.txt"}`)
	if res.IsError {
		t.Fatalf("delete failed: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone")
	}
}

func TestDelete_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "d"), 0o750)
	res := DeleteTool(dir, 0).Call(context.Background(), `{"path":"d"}`)
	if res.IsError {
		t.Fatalf("delete dir failed: %s", res.Output)
	}
}

func TestDelete_NonEmptyDirRejected(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "d"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "d", "f.txt"), []byte("x"), 0o600)
	res := DeleteTool(dir, 0).Call(context.Background(), `{"path":"d"}`)
	if !res.IsError || !strings.Contains(res.Output, "not empty") {
		t.Errorf("non-empty dir should be rejected: %s", res.Output)
	}
}

func TestDelete_GlobRejected(t *testing.T) {
	res := DeleteTool(t.TempDir(), 0).Call(context.Background(), `{"path":"*.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "glob") {
		t.Errorf("glob path should be rejected: %s", res.Output)
	}
}

func TestDelete_EscapeRejected(t *testing.T) {
	res := DeleteTool(t.TempDir(), 0).Call(context.Background(), `{"path":"../x.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "default mode") {
		t.Errorf("escape should be rejected: %s", res.Output)
	}
}

func TestDelete_RootRejected(t *testing.T) {
	dir := t.TempDir()
	res := DeleteTool(dir, 0).Call(context.Background(), `{"path":"."}`)
	if !res.IsError {
		t.Errorf("workdir root delete should be rejected: %s", res.Output)
	}
}

// Deleting inside .git must be rejected: it bypasses the git tool's allow-list (removing
// hooks/config/refs corrupts repo state the git tool then operates on).
func TestDelete_DotGitRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".git/config", ".git/hooks/pre-commit", "sub/.git/HEAD"} {
		res := DeleteTool(dir, 0).Call(context.Background(), `{"path":"`+p+`"}`)
		if !res.IsError || !strings.Contains(res.Output, ".git") {
			t.Errorf("%s delete should be rejected with .git message: %s", p, res.Output)
		}
	}
}

func TestDelete_MissingParam(t *testing.T) {
	res := DeleteTool(t.TempDir(), 0).Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing parameter") {
		t.Errorf("expected missing parameter error, got: %s", res.Output)
	}
}

func TestDelete_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	_ = os.WriteFile(target, []byte("x"), 0o600)
	_ = os.Symlink(target, filepath.Join(dir, "link.txt"))
	res := DeleteTool(dir, 0).Call(context.Background(), `{"path":"link.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "symlink") {
		t.Errorf("symlink delete should be rejected: %s", res.Output)
	}
}
