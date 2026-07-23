package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return dir
}

func TestReadFile_RelativePath(t *testing.T) {
	dir := writeTemp(t, "a.txt", "hello world")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"a.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "hello world" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_AbsoluteInsideRoot(t *testing.T) {
	dir := writeTemp(t, "b.txt", "abs ok")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"`+filepath.Join(dir, "b.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "abs ok" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_Range(t *testing.T) {
	dir := writeTemp(t, "r.txt", "line1\nline2\nline3\n")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"r.txt","offset":2,"limit":1}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "2 │ line2") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_Truncates(t *testing.T) {
	long := strings.Repeat("a", maxReadFileChars+500)
	dir := writeTemp(t, "big.txt", long)
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"big.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "…") {
		t.Errorf("missing truncation marker")
	}
}

func TestWriteFile_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"a.txt","content":"hello"}`)
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
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"src/nested/deep/c.go","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "nested", "deep", "c.go")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWriteFile_FileMode0644(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"m.txt","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	info, _ := os.Stat(filepath.Join(dir, "m.txt"))
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
}

// 写超过 10MiB 上限：参数校验阶段拒绝，不进入 IO。
func TestWriteFile_RejectsOversized(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", maxWriteFileBytes+1)
	res := WriteFileTool(dir).Call(context.Background(), `{"path":"big.bin","content":"`+big+`"}`)
	if !res.IsError {
		t.Fatal("expected oversize error")
	}
}

func TestEditFile_UniqueReplacement(t *testing.T) {
	dir := writeTemp(t, "e.txt", "hello world")
	res := EditFileTool(dir).Call(context.Background(), `{"path":"e.txt","old_string":"hello","new_string":"hi"}`)
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
	res := EditFileTool(dir).Call(context.Background(), `{"path":"e.txt","old_string":"xyz","new_string":"abc"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestEditFile_MultipleMatchesFails(t *testing.T) {
	dir := writeTemp(t, "e.txt", "xx yy xx")
	res := EditFileTool(dir).Call(context.Background(), `{"path":"e.txt","old_string":"xx","new_string":"z"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// edit_file 通过 openNoFollow 拒绝最终路径是符号链接的情形。
func TestEditFile_SymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := EditFileTool(dir).Call(context.Background(), `{"path":"link","old_string":"hello","new_string":"hi"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	got, _ := os.ReadFile(filepath.Join(outside, "target.txt"))
	if string(got) != "hello world" {
		t.Errorf("outside file was modified: %q", got)
	}
}

// 已取消的 ctx 应让文件工具立即返回，不进入 IO。
func TestFileTools_RespectCancelledCtx(t *testing.T) {
	dir := writeTemp(t, "x.txt", "hello")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tool := range map[string]Tool{
		"read_file":  ReadFileTool(dir),
		"write_file": WriteFileTool(dir),
		"edit_file":  EditFileTool(dir),
	} {
		args := `{"path":"x.txt"}`
		switch name {
		case "write_file":
			args = `{"path":"x.txt","content":"y"}`
		case "edit_file":
			args = `{"path":"x.txt","old_string":"h","new_string":"H"}`
		}
		res := tool.Call(ctx, args)
		if !res.IsError {
			t.Errorf("%s: expected error on cancelled ctx", name)
		}
		if !strings.Contains(res.Output, "已取消") {
			t.Errorf("%s: error = %q", name, res.Output)
		}
	}
}

// workdir 为空：read_file 用相对路径仍能工作（依赖调用方进程 cwd）。
func TestReadFile_EmptyWorkdir(t *testing.T) {
	dir := writeTemp(t, "cwd.txt", "from-cwd")
	res := ReadFileTool("").Call(context.Background(), `{"path":"`+filepath.Join(dir, "cwd.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "from-cwd" {
		t.Errorf("Output = %q", res.Output)
	}
}
