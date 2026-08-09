package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// §P1-A sanitizeFileSegment: compresses into a filename-safe segment.
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

// §P1-A bound table-driven: fits (no spill), overflows (spill + path hint), readonly (best-effort fallback preview).
func TestBound_Table(t *testing.T) {
	t.Run("fits_no_spill", func(t *testing.T) {
		dir := t.TempDir()
		s := newToolOutputStore(dir, 0, nil)
		got := s.bound(1, "c1", "short", "short", false)
		if got != "short" {
			t.Errorf("truncated=false should return preview as-is: got %q", got)
		}
		if files, _ := os.ReadDir(dir); len(files) != 0 {
			t.Errorf("truncated=false should not spill to disk: %d files", len(files))
		}
	})

	t.Run("overflows_spills_full", func(t *testing.T) {
		dir := t.TempDir()
		s := newToolOutputStore(dir, 0, nil)
		full := strings.Repeat("x", 5000)
		preview := "head-preview"
		got := s.bound(2, "call-1", full, preview, true)
		if !strings.Contains(got, preview) {
			t.Errorf("return should contain preview: %q", got)
		}
		if !strings.Contains(got, "saved") || !strings.Contains(got, dir) {
			t.Errorf("return should contain path hint + directory: %q", got)
		}
		// Spilled file exists and content == full text.
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("should spill exactly 1 file: %d", len(entries))
		}
		b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			t.Fatalf("read spill file: %v", err)
		}
		if string(b) != full {
			t.Errorf("spilled content should == full text: len got=%d want=%d", len(b), len(full))
		}
	})

	t.Run("readonly_dir_falls_back", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("readonly permission test is reliable only in non-root POSIX environments")
		}
		dir := t.TempDir()
		// Use a non-existent subdir + readonly parent to make MkdirAll fail.
		rd := filepath.Join(dir, "ro")
		if err := os.MkdirAll(rd, 0o500); err != nil {
			t.Fatalf("mkdir ro: %v", err)
		}
		target := filepath.Join(rd, "nested")
		s := newToolOutputStore(target, 0, nil)
		full := strings.Repeat("y", 5000)
		got := s.bound(1, "c", full, "preview", true)
		if strings.Contains(got, "saved") {
			t.Errorf("readonly dir should best-effort fallback without marker: %q", got)
		}
	})
}

// §P1-A bound byte cap (review Finding 1): CJK output exceeding 1 MiB must be truncated at rune boundaries (valid UTF-8),
// the marker honestly annotates "partially truncated" (does not claim completeness).
func TestBound_ByteCapRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	s := newToolOutputStore(dir, 0, nil)
	big := strings.Repeat("中", 350*1024) // 3 bytes/rune × 350k ≈ 1.05 MiB > toolOutputMaxBytes
	got := s.bound(1, "c", big, "preview", true)
	if !strings.Contains(got, "partially truncated") {
		t.Errorf(`when byte cap triggered, marker should contain "partially truncated": %q`, got)
	}
	if strings.Contains(got, "full output saved") {
		t.Errorf(`when byte cap triggered, should not claim "complete": %q`, got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("should spill 1 file: %d", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if !utf8.Valid(b) {
		t.Errorf("spilled file should be valid UTF-8 (truncated at rune boundary): invalid, len=%d", len(b))
	}
	if len(b) >= len(big) {
		t.Errorf("output exceeding 1 MiB should be truncated: got len=%d, orig=%d", len(b), len(big))
	}
	if len(b) > toolOutputMaxBytes {
		t.Errorf("after truncation should not exceed toolOutputMaxBytes: len=%d", len(b))
	}
}

// §P1-A cleanup: deletes only tool_*.txt whose mtime < now-retention.
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
		t.Errorf("old (mtime -8d) should be deleted: stat err=%v", err)
	}
	if _, err := os.Stat(newf); err != nil {
		t.Errorf("new (mtime now) should be retained: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("non tool_*.txt should not be cleaned: %v", err)
	}
}

// §P1-3 cleanup count cap: beyond toolOutputMaxFiles, evicts oldest-mtime-first even when files are within retention
// ("未过期也删"). The newest files survive — protecting the read-back contract for hints the model may still act on.
func TestToolOutputStore_Cleanup_CountCap(t *testing.T) {
	dir := t.TempDir()
	const n = toolOutputMaxFiles + 2
	base := time.Now().Add(-1 * time.Hour) // well within the 7d retention below → only the count cap should fire
	for i := range n {
		path := filepath.Join(dir, fmt.Sprintf("tool_%03d.txt", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := base.Add(time.Duration(i) * time.Second) // i=0 oldest, i=n-1 newest
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	s := newToolOutputStore(dir, 7*24*time.Hour, nil)
	s.cleanup()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != toolOutputMaxFiles {
		t.Fatalf("count cap should leave exactly toolOutputMaxFiles files: got %d want %d", len(entries), toolOutputMaxFiles)
	}
	// Oldest two evicted (oldest-mtime-first) ...
	if _, err := os.Stat(filepath.Join(dir, "tool_000.txt")); !os.IsNotExist(err) {
		t.Errorf("oldest tool_000 should be evicted: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tool_001.txt")); !os.IsNotExist(err) {
		t.Errorf("second-oldest tool_001 should be evicted: stat err=%v", err)
	}
	// ... and the newest retained (read-back contract).
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("tool_%03d.txt", n-1))); err != nil {
		t.Errorf("newest file should be retained (read-back contract): %v", err)
	}
}
