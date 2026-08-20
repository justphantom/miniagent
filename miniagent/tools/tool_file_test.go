package tools

import (
	"context"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return dir
}

func TestWriteFile_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"a.txt","content":"hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "hello" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteFile_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"src/nested/deep/c.go","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "nested", "deep", "c.go")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWriteFile_NewFileDefaultPerm0600(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"m.txt","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	info, _ := os.Stat(filepath.Join(dir, "m.txt"))
	// New files default to owner-only 0600 (S-7: tighter than the historical 0644 for multi-user hosts).
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new-file mode = %o, want 0600", info.Mode().Perm())
	}
}

// Overwrite preserves the existing file's permissions (not the new-file 0600 default).
func TestWriteFile_OverwritePreservesPerm(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "exist.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"exist.txt","content":"new"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o644 {
		t.Errorf("overwritten file mode = %o, want preserved 0644", info.Mode().Perm())
	}
}

// Writing beyond the 10MiB limit: rejected at the argument-validation stage, never entering IO.
func TestWriteFile_RejectsOversized(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", maxWriteFileBytes+1)
	res := WriteFileTool(dir, 0).Call(context.Background(), `{"path":"big.bin","content":"`+big+`"}`)
	if !res.IsError {
		t.Fatal("expected oversize error")
	}
}

func TestEditFile_UniqueReplacement(t *testing.T) {
	dir := writeTemp(t, "e.txt", "hello world")
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"e.txt","old_string":"hello","new_string":"hi"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "e.txt"))
	if string(got) != "hi world" {
		t.Errorf("content = %q", got)
	}
}

func TestEditFile_ZeroMatchesFails(t *testing.T) {
	dir := writeTemp(t, "e.txt", "hello world")
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"e.txt","old_string":"xyz","new_string":"abc"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestEditFile_MultipleMatchesFails(t *testing.T) {
	dir := writeTemp(t, "e.txt", "xx yy xx")
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"e.txt","old_string":"xx","new_string":"z"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// B-3: replace_all=true replaces every match.
func TestEditFile_ReplaceAll(t *testing.T) {
	dir := writeTemp(t, "e.txt", "xx yy xx")
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"e.txt","old_string":"xx","new_string":"z","replace_all":true}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "e.txt"))
	if string(got) != "z yy z" {
		t.Errorf("content = %q, want %q", got, "z yy z")
	}
}

// replace_all=true with no matches still errors.
func TestEditFile_ReplaceAllZeroMatchFails(t *testing.T) {
	dir := writeTemp(t, "e.txt", "hello")
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"e.txt","old_string":"zz","new_string":"z","replace_all":true}`)
	if !res.IsError {
		t.Fatal("expected error for zero matches")
	}
}

// edit rejects a symlink as the final path via openNoFollow.
func TestEditFile_SymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"link","old_string":"hello","new_string":"hi"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	got, _ := os.ReadFile(filepath.Join(outside, "target.txt"))
	if string(got) != "hello world" {
		t.Errorf("outside file was modified: %q", got)
	}
}

// edit must also reject non-regular files: a FIFO would make openNoFollow block permanently (edit has no
// built-in timeout), which then exhausts the parallelism semaphore and hangs the whole Run.
func TestEditFile_RejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	res := EditFileTool(dir, 0).Call(context.Background(), `{"path":"fifo","old_string":"x","new_string":"y"}`)
	if !res.IsError {
		t.Fatal("expected FIFO to be rejected")
	}
	if !strings.Contains(res.Output, "regular file") {
		t.Errorf("Output = %q", res.Output)
	}
}

// A already-cancelled ctx should make the file tools return immediately, without entering IO.
func TestFileTools_RespectCancelledCtx(t *testing.T) {
	dir := writeTemp(t, "x.txt", "hello")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tool := range map[string]miniagent.Tool{
		"read":  ReadFileTool(dir, 0, 0),
		"write": WriteFileTool(dir, 0),
		"edit":  EditFileTool(dir, 0),
	} {
		args := `{"path":"x.txt"}`
		switch name {
		case "write":
			args = `{"path":"x.txt","content":"y"}`
		case "edit":
			args = `{"path":"x.txt","old_string":"h","new_string":"H"}`
		}
		res := tool.Call(ctx, args)
		if !res.IsError {
			t.Errorf("%s: expected error on cancelled ctx", name)
		}
		if !strings.Contains(res.Output, "cancelled") {
			t.Errorf("%s: error = %q", name, res.Output)
		}
	}
}
