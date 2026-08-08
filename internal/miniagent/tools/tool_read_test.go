package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadFile_RelativePath(t *testing.T) {
	dir := writeTemp(t, "a.txt", "hello world")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"a.txt"}`)
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
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"`+filepath.Join(dir, "b.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 │ abs ok") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_Range(t *testing.T) {
	dir := writeTemp(t, "r.txt", "line1\nline2\nline3\n")
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"r.txt","offset":2,"limit":1}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "2 │ line2") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestReadFile_Truncates(t *testing.T) {
	long := strings.Repeat("a", maxReadFileBytes/4+500)
	dir := writeTemp(t, "big.txt", long)
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"big.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "…") {
		t.Errorf("missing truncation marker")
	}
}

// P2-3：read 用 Lstat 判定文件类型，对最终路径分量是符号链接直接拒（与 edit/openNoFollow
// 对齐）。旧 Stat 会跟随软链读 target，与「拒绝符号链接」描述不符。中间目录 symlink
// 仍跟随（read 无路径约束，仅最终分量由 Lstat/O_NOFOLLOW 兜底）。
func TestReadFile_SymlinkFinalComponentRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "real.txt")
	if err := os.WriteFile(target, []byte("via symlink"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"link"}`)
	if !res.IsError {
		t.Fatal("expected symlink final component to be rejected")
	}
	if !strings.Contains(res.Output, "普通文件") {
		t.Errorf("Output = %q", res.Output)
	}
	// target 内容不应被读出（防软链读绕过到 workdir 外）。
	if strings.Contains(res.Output, "via symlink") {
		t.Errorf("symlink target content leaked: %q", res.Output)
	}
}

// workdir 为空：read 用相对路径仍能工作（依赖调用方进程 cwd）。
func TestReadFile_EmptyWorkdir(t *testing.T) {
	dir := writeTemp(t, "cwd.txt", "from-cwd")
	res := ReadFileTool("", 0, 0).Call(context.Background(), `{"path":"`+filepath.Join(dir, "cwd.txt")+`"}`)
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
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"small.txt","offset":42}`)
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
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"empty.txt"}`)
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
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"fifo"}`)
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
	res := ReadFileTool("", 0, 0).Call(context.Background(), `{"path":"/dev/null"}`)
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
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"bin.dat"}`)
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
	res := ReadFileTool(dir, 0, 0).Call(context.Background(), `{"path":"late.txt"}`)
	if res.IsError {
		t.Fatalf("late-NUL text should pass: %s", res.Output)
	}
}
