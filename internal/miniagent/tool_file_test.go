package miniagent

import (
	"context"
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

func TestReadFile_RelativePath(t *testing.T) {
	dir := writeTemp(t, "a.txt", "hello world")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"a.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// 默认输出带行号（edit 依赖行号定位）。
	if !strings.Contains(res.Output, "1 │ hello world") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_AbsoluteInsideRoot(t *testing.T) {
	dir := writeTemp(t, "b.txt", "abs ok")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"`+filepath.Join(dir, "b.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 │ abs ok") {
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

// B-3：replace_all=true 替换全部匹配处。
func TestEditFile_ReplaceAll(t *testing.T) {
	dir := writeTemp(t, "e.txt", "xx yy xx")
	res := EditFileTool(dir).Call(context.Background(), `{"path":"e.txt","old_string":"xx","new_string":"z","replace_all":true}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "e.txt"))
	if string(got) != "z yy z" {
		t.Errorf("content = %q, want %q", got, "z yy z")
	}
}

// replace_all=true 但无匹配仍报错。
func TestEditFile_ReplaceAllZeroMatchFails(t *testing.T) {
	dir := writeTemp(t, "e.txt", "hello")
	res := EditFileTool(dir).Call(context.Background(), `{"path":"e.txt","old_string":"zz","new_string":"z","replace_all":true}`)
	if !res.IsError {
		t.Fatal("expected error for zero matches")
	}
}

// edit 通过 openNoFollow 拒绝最终路径是符号链接的情形。
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

// edit 同样必须拒绝非 regular 文件：FIFO 会让 openNoFollow 永久阻塞（edit 无
// 内置超时），进而占满并行信号量使整个 Run 挂死。
func TestEditFile_RejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	res := EditFileTool(dir).Call(context.Background(), `{"path":"fifo","old_string":"x","new_string":"y"}`)
	if !res.IsError {
		t.Fatal("expected FIFO to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 已取消的 ctx 应让文件工具立即返回，不进入 IO。
func TestFileTools_RespectCancelledCtx(t *testing.T) {
	dir := writeTemp(t, "x.txt", "hello")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tool := range map[string]Tool{
		"read":  ReadFileTool(dir),
		"write": WriteFileTool(dir),
		"edit":  EditFileTool(dir),
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
		if !strings.Contains(res.Output, "已取消") {
			t.Errorf("%s: error = %q", name, res.Output)
		}
	}
}

// workdir 为空：read 用相对路径仍能工作（依赖调用方进程 cwd）。
func TestReadFile_EmptyWorkdir(t *testing.T) {
	dir := writeTemp(t, "cwd.txt", "from-cwd")
	res := ReadFileTool("").Call(context.Background(), `{"path":"`+filepath.Join(dir, "cwd.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 │ from-cwd") {
		t.Errorf("Output = %q", res.Output)
	}
}

// offset 超过文件行数应作为 IsError 返回（而非静默空输出）。
func TestReadFile_OffsetOutOfBoundsIsError(t *testing.T) {
	dir := writeTemp(t, "small.txt", "only one line")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"small.txt","offset":42}`)
	if !res.IsError {
		t.Fatal("expected error for offset out of bounds")
	}
	if !strings.Contains(res.Output, "offset 42") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 空文件不应输出"1 │ "伪空行，直接返回空串。
func TestReadFile_EmptyFileReturnsBlank(t *testing.T) {
	dir := writeTemp(t, "empty.txt", "")
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"empty.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "" {
		t.Errorf("Output = %q, want empty", res.Output)
	}
}

// 非 regular 文件（FIFO/设备/socket）必须拒绝，否则 FIFO 会无限阻塞 open。
func TestReadFile_RejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"fifo"}`)
	if !res.IsError {
		t.Fatal("expected FIFO to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
}

// /dev/null 是 character device，应拒绝（而非返回空内容）。
func TestReadFile_RejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	res := ReadFileTool("").Call(context.Background(), `{"path":"/dev/null"}`)
	if !res.IsError {
		t.Fatal("expected device file to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
}

// 二进制内容（含 NUL）必须拒绝，避免乱码污染 LLM 上下文。
func TestReadFile_RejectsBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(bin, []byte("ABC\x00\x01DEF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"bin.dat"}`)
	if !res.IsError {
		t.Fatal("expected binary to be rejected")
	}
	if !strings.Contains(res.Output, "二进制") {
		t.Errorf("Output = %q", res.Output)
	}
}

// NUL 出现在 8 KiB 扫描窗口之外（>8192 字节）的纯文本应正常读取。
func TestReadFile_TextWithLateNULPasses(t *testing.T) {
	dir := t.TempDir()
	// 9000 字节纯文本 + 末尾一个 NUL：扫描窗口只看前 8192，应放行。
	content := strings.Repeat("a", 9000) + "\x00"
	bin := filepath.Join(dir, "late.txt")
	if err := os.WriteFile(bin, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	res := ReadFileTool(dir).Call(context.Background(), `{"path":"late.txt"}`)
	if res.IsError {
		t.Fatalf("late-NUL text should pass: %s", res.Output)
	}
}
