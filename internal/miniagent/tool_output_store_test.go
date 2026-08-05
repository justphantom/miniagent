package miniagent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// §P1-A sanitizeFileSegment：压成文件名安全段。
func TestSanitizeFileSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"synth_1_2", "synth_1_2"},
		{"a/b", "a_b"},
		{"..", "__"},
		{"p@id!", "p_id_"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeFileSegment(c.in); got != c.want {
			t.Errorf("sanitizeFileSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// §P1-A bound 表驱动：fits（无落盘）、overflows（落盘+路径提示）、readonly（best-effort 回落预览）。
func TestBound_Table(t *testing.T) {
	t.Run("fits_no_spill", func(t *testing.T) {
		dir := t.TempDir()
		s := newToolOutputStore(dir, 0, nil)
		got := s.bound(1, "c1", "short", "short", false)
		if got != "short" {
			t.Errorf("truncated=false 应原样返回 preview: got %q", got)
		}
		if files, _ := os.ReadDir(dir); len(files) != 0 {
			t.Errorf("truncated=false 不应落盘: %d files", len(files))
		}
	})

	t.Run("overflows_spills_full", func(t *testing.T) {
		dir := t.TempDir()
		s := newToolOutputStore(dir, 0, nil)
		full := strings.Repeat("x", 5000)
		preview := "head-preview"
		got := s.bound(2, "call-1", full, preview, true)
		if !strings.Contains(got, preview) {
			t.Errorf("返回应含 preview: %q", got)
		}
		if !strings.Contains(got, "已保存") || !strings.Contains(got, dir) {
			t.Errorf("返回应含路径提示 + 目录: %q", got)
		}
		// 落盘文件存在且内容 == 全文。
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("应恰好落盘 1 个文件: %d", len(entries))
		}
		b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			t.Fatalf("read spill file: %v", err)
		}
		if string(b) != full {
			t.Errorf("落盘内容应 == 全文: len got=%d want=%d", len(b), len(full))
		}
	})

	t.Run("readonly_dir_falls_back", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("readonly 权限测试仅在非 root 的 POSIX 环境可靠")
		}
		dir := t.TempDir()
		// 用一个不存在的子目录 + 只读父目录，使 MkdirAll 失败。
		rd := filepath.Join(dir, "ro")
		if err := os.MkdirAll(rd, 0o500); err != nil {
			t.Fatalf("mkdir ro: %v", err)
		}
		target := filepath.Join(rd, "nested")
		s := newToolOutputStore(target, 0, nil)
		full := strings.Repeat("y", 5000)
		got := s.bound(1, "c", full, "preview", true)
		if strings.Contains(got, "已保存") {
			t.Errorf("只读目录应 best-effort 回落不带 marker: %q", got)
		}
	})
}

// §P1-A bound 字节封顶（review Finding 1）：超 1 MiB 的 CJK 输出须在 rune 边界截断（合法 UTF-8），
// marker 诚实标注「部分已截断」（不声称完整）。
func TestBound_ByteCapRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	s := newToolOutputStore(dir, 0, nil)
	big := strings.Repeat("中", 350*1024) // 3 字节/rune × 350k ≈ 1.05 MiB > toolOutputMaxBytes
	got := s.bound(1, "c", big, "preview", true)
	if !strings.Contains(got, "部分已截断") {
		t.Errorf("byte cap 触发时 marker 应含「部分已截断」: %q", got)
	}
	if strings.Contains(got, "完整输出已保存") {
		t.Errorf("byte cap 触发时不应声称「完整」: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("应落盘 1 文件: %d", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if !utf8.Valid(b) {
		t.Errorf("落盘文件应是合法 UTF-8（rune 边界截断）: invalid, len=%d", len(b))
	}
	if len(b) >= len(big) {
		t.Errorf("超 1 MiB 应被截断: got len=%d, orig=%d", len(b), len(big))
	}
	if len(b) > toolOutputMaxBytes {
		t.Errorf("截断后不应超 toolOutputMaxBytes: len=%d", len(b))
	}
}

// §P1-A cleanup：仅删 mtime < now-retention 的 tool_*.txt。
func TestToolOutputStore_Cleanup(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "tool_old.txt")
	newf := filepath.Join(dir, "tool_new.txt")
	other := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(old, []byte("o"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newf, []byte("n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	s := newToolOutputStore(dir, 7*24*time.Hour, nil)
	s.cleanup()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old (mtime -8d) 应被删除: stat err=%v", err)
	}
	if _, err := os.Stat(newf); err != nil {
		t.Errorf("new (mtime now) 应保留: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("非 tool_*.txt 不应被清理: %v", err)
	}
}
