package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// writeMemoryTool 追加 {type:note} 记录，readMemoryTool 渲染为文本；round-trip + 落盘到 .miniagent/memory.jsonl。
func TestMemory_WriteThenRead(t *testing.T) {
	dir := t.TempDir()
	if r := writeMemoryTool(dir, "避免在测试里 time.Sleep"); r.IsError {
		t.Fatalf("write: %s", r.Output)
	}
	if r := writeMemoryTool(dir, "client.go 是 HTTP 层"); r.IsError {
		t.Fatalf("write2: %s", r.Output)
	}
	// 文件落盘到 .miniagent/memory.jsonl，0o600。
	path := filepath.Join(dir, ".miniagent", "memory.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("memory file not written: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("memory file mode = %o, want 600", info.Mode().Perm())
	}
	if !strings.Contains(string(data), `"type":"note"`) || !strings.Contains(string(data), "避免在测试里 time.Sleep") {
		t.Errorf("memory jsonl missing record: %s", data)
	}
	// readMemoryTool 渲染两条记录。
	r := readMemoryTool(dir)
	if r.IsError {
		t.Fatalf("read: %s", r.Output)
	}
	if !strings.Contains(r.Output, "[note]") || !strings.Contains(r.Output, "避免在测试里 time.Sleep") || !strings.Contains(r.Output, "client.go 是 HTTP 层") {
		t.Errorf("read rendered wrong: %s", r.Output)
	}
}

// 无 memory 文件 → readMemoryTool 给出友好提示而非报错。
func TestMemory_ReadEmpty(t *testing.T) {
	r := readMemoryTool(t.TempDir())
	if r.IsError || !strings.Contains(r.Output, "暂无") {
		t.Errorf("empty read should be friendly non-error: %+v", r)
	}
}

// writeMemoryTool 空 content 报错。
func TestMemory_WriteEmptyRejected(t *testing.T) {
	if r := writeMemoryTool(t.TempDir(), "   "); !r.IsError {
		t.Error("empty content should be rejected")
	}
}

// FormatMemorySnippet 只注入最近 memoryRecentN 条。
func TestMemory_FormatSnippetLastN(t *testing.T) {
	dir := t.TempDir()
	for i := range memoryRecentN + 2 {
		if err := appendMemoryRecord(dir, memoryRecord{Type: "lesson", Topic: "t", Content: "c" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	snip := FormatMemorySnippet(dir)
	if !strings.Contains(snip, "## 近期项目记忆") {
		t.Errorf("snippet missing header: %s", snip)
	}
	// 旧记录 c0、c1 不在最近 N（memoryRecentN）内。
	if strings.Contains(snip, "c0\n") || strings.Contains(snip, "c0 ") {
		t.Errorf("snippet should drop oldest beyond last N: %s", snip)
	}
	if !strings.Contains(snip, "c"+strconv.Itoa(memoryRecentN+1)) {
		t.Errorf("snippet should include newest: %s", snip)
	}
}

// FormatMemorySnippet 无记忆返回空串（不污染 system prompt）。
func TestMemory_FormatSnippetEmpty(t *testing.T) {
	if got := FormatMemorySnippet(t.TempDir()); got != "" {
		t.Errorf("empty snippet should be \"\", got %q", got)
	}
}

// read/write 工具的保留路径 "memory" 路由到项目记忆（绕过普通文件路径）。
func TestReadWriteTools_MemoryTokenRouting(t *testing.T) {
	dir := t.TempDir()
	w := WriteFileTool(dir, 0)
	if r := w.Call(context.Background(), `{"path":"memory","content":"一条记忆"}`); r.IsError {
		t.Fatalf("write memory: %s", r.Output)
	}
	r := ReadFileTool(dir, 0).Call(context.Background(), `{"path":"memory"}`)
	if r.IsError {
		t.Fatalf("read memory: %s", r.Output)
	}
	if !strings.Contains(r.Output, "一条记忆") {
		t.Errorf("read memory did not route to memory file: %s", r.Output)
	}
}

// .miniagent 目录为符号链接时，read/write memory 均须拒绝，防止越界读写。
func TestMemory_SymlinkDirRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, ".miniagent")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if r := readMemoryTool(dir); !r.IsError {
		t.Errorf("read should reject symlink .miniagent dir")
	}
	if r := writeMemoryTool(dir, "x"); !r.IsError {
		t.Errorf("write should reject symlink .miniagent dir")
	}
}

// memory.jsonl 自身为符号链接时，read/write memory 均须拒绝。
func TestMemory_SymlinkFileRejected(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "real.jsonl")
	if err := os.WriteFile(outside, []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(maDir, "memory.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if r := readMemoryTool(dir); !r.IsError {
		t.Errorf("read should reject symlink memory file")
	}
	if r := writeMemoryTool(dir, "x"); !r.IsError {
		t.Errorf("write should reject symlink memory file")
	}
}

// memory.jsonl 为非普通文件（如 FIFO）时须拒绝。
func TestMemory_NonRegularFileRejected(t *testing.T) {
	dir := t.TempDir()
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o750); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(maDir, "memory.jsonl")
	if err := os.WriteFile(fifo, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 先写一次正常文件确保流程通，再换成 FIFO
	_ = os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	if r := readMemoryTool(dir); !r.IsError {
		t.Errorf("read should reject FIFO memory file")
	}
	if r := writeMemoryTool(dir, "x"); !r.IsError {
		t.Errorf("write should reject FIFO memory file")
	}
}
