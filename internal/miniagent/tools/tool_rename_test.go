package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRename_File(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o600)
	res := RenameTool(dir, 0).Call(context.Background(), `{"from":"a.txt","to":"b.txt"}`)
	if res.IsError {
		t.Fatalf("rename failed: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatalf("destination not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("source should not exist anymore")
	}
}

func TestRename_Directory(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "olddir"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "olddir", "f.txt"), []byte("x"), 0o600)
	res := RenameTool(dir, 0).Call(context.Background(), `{"from":"olddir","to":"newdir"}`)
	if res.IsError {
		t.Fatalf("rename dir failed: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "newdir", "f.txt")); err != nil {
		t.Fatalf("file should exist in newdir: %v", err)
	}
}

func TestRename_EscapeRejected(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o600)
	for _, c := range []struct{ name, args string }{
		{"from escape", `{"from":"../a.txt","to":"b.txt"}`},
		{"to escape", `{"from":"a.txt","to":"../b.txt"}`},
	} {
		res := RenameTool(dir, 0).Call(context.Background(), c.args)
		if !res.IsError || !strings.Contains(res.Output, "default mode") {
			t.Errorf("%s should be rejected: %s", c.name, res.Output)
		}
	}
}

func TestRename_SymlinkSourceRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	_ = os.WriteFile(target, []byte("x"), 0o600)
	link := filepath.Join(dir, "link.txt")
	_ = os.Symlink(target, link)
	res := RenameTool(dir, 0).Call(context.Background(), `{"from":"link.txt","to":"z.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "symlink") {
		t.Errorf("symlink source should be rejected: %s", res.Output)
	}
}

func TestRename_MissingParam(t *testing.T) {
	res := RenameTool(t.TempDir(), 0).Call(context.Background(), `{"from":"a.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "missing parameter") {
		t.Errorf("expected missing parameter error, got: %s", res.Output)
	}
}

func TestRename_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600)
	res := RenameTool(dir, 0).Call(context.Background(), `{"from":"a.txt","to":"sub/b.txt"}`)
	if res.IsError {
		t.Fatalf("rename with parent dir creation failed: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "b.txt")); err != nil {
		t.Fatalf("destination not found: %v", err)
	}
}
