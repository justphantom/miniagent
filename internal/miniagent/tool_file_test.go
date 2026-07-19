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
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"a.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "hello world" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_AbsoluteInsideRoot(t *testing.T) {
	dir := writeTemp(t, "b.txt", "abs ok")
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"`+filepath.Join(dir, "b.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "abs ok" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_EscapeViaDotDot(t *testing.T) {
	dir := writeTemp(t, "c.txt", "x")
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"../../../etc/passwd"}`)
	if !res.IsError {
		t.Fatalf("expected error")
	}
}

func TestReadFile_SymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "secret.txt"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"link/secret.txt"}`)
	if !res.IsError {
		t.Fatalf("expected error")
	}
	if strings.Contains(res.Output, "TOPSECRET") {
		t.Errorf("secret leaked")
	}
}

func TestReadFile_Range(t *testing.T) {
	dir := writeTemp(t, "r.txt", "line1\nline2\nline3\n")
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"r.txt","offset":2,"limit":1}`)
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
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"big.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "…") {
		t.Errorf("missing truncation marker")
	}
}

func TestWriteFile_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir, false).Call(context.Background(), `{"path":"a.txt","content":"hello"}`)
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
	res := WriteFileTool(dir, false).Call(context.Background(), `{"path":"src/nested/deep/c.go","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "nested", "deep", "c.go")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWriteFile_EscapeRejected(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir, false).Call(context.Background(), `{"path":"../../../tmp/evil","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestWriteFile_FileMode0644(t *testing.T) {
	dir := t.TempDir()
	res := WriteFileTool(dir, false).Call(context.Background(), `{"path":"m.txt","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	info, _ := os.Stat(filepath.Join(dir, "m.txt"))
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
}

func TestEditFile_UniqueReplacement(t *testing.T) {
	dir := writeTemp(t, "e.txt", "hello world")
	res := EditFileTool(dir, false).Call(context.Background(), `{"path":"e.txt","old_string":"hello","new_string":"hi"}`)
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
	res := EditFileTool(dir, false).Call(context.Background(), `{"path":"e.txt","old_string":"xyz","new_string":"abc"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestEditFile_MultipleMatchesFails(t *testing.T) {
	dir := writeTemp(t, "e.txt", "xx yy xx")
	res := EditFileTool(dir, false).Call(context.Background(), `{"path":"e.txt","old_string":"xx","new_string":"z"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestWriteFile_SymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := WriteFileTool(dir, false).Call(context.Background(), `{"path":"link","content":"pwned"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	got, _ := os.ReadFile(filepath.Join(outside, "target.txt"))
	if string(got) != "ORIGINAL" {
		t.Errorf("outside file was modified: %q", got)
	}
}

// 父目录是符号链接指向外部：第一次 resolveUnderRoot 因父目录存在会被
// EvalSymlinks 解析并拦截；该测试同时覆盖 MkdirAll 后的二次校验路径。
func TestWriteFile_SymlinkParentDirRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	// workspace 内的 subd 是指向外部目录的符号链接。
	if err := os.Symlink(outside, filepath.Join(dir, "subd")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := WriteFileTool(dir, false).Call(context.Background(), `{"path":"subd/evil.txt","content":"pwned"}`)
	if !res.IsError {
		t.Fatal("expected error for symlinked parent dir")
	}
	// 外部目录不应被写入。
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Errorf("outside file was created via symlink parent")
	}
}

func TestEditFile_SymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := EditFileTool(dir, false).Call(context.Background(), `{"path":"link","old_string":"hello","new_string":"hi"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	got, _ := os.ReadFile(filepath.Join(outside, "target.txt"))
	if string(got) != "hello world" {
		t.Errorf("outside file was modified: %q", got)
	}
}

// EvalSymlinks 失败（循环链接）必须返回 error，不能静默放行。
func TestReadFile_CyclicSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := ReadFileTool(dir, false).Call(context.Background(), `{"path":"a"}`)
	if !res.IsError {
		t.Fatal("expected error for cyclic symlink")
	}
}

// 已取消的 ctx 应让文件工具立即返回，不进入 IO。
func TestFileTools_RespectCancelledCtx(t *testing.T) {
	dir := writeTemp(t, "x.txt", "hello")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tool := range map[string]Tool{
		"read_file":  ReadFileTool(dir, false),
		"write_file": WriteFileTool(dir, false),
		"edit_file":  EditFileTool(dir, false),
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
