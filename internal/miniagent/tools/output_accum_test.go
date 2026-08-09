package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §P1-D under-limit: writes a small amount (< keep) → finalize has no banner, join is equivalent to the original.
func TestOutputAccum_UnderLimit(t *testing.T) {
	a := newOutputAccum(100*1024, 0, "", "")
	if err := a.write("hello "); err != nil {
		t.Fatal(err)
	}
	if err := a.write("world"); err != nil {
		t.Fatal(err)
	}
	got := a.finalize(5000)
	if got != "hello world" {
		t.Errorf("under-limit finalize = %q, want %q", got, "hello world")
	}
}

// §P1-D over-limit-keeps-tail: exceeds keep, drops the middle and keeps the tail; finalize contains the banner, the tail, not the head.
func TestOutputAccum_OverLimitKeepsTail(t *testing.T) {
	a := newOutputAccum(100*1024, 0, "", "") // keep=100KB
	firstChunk := "F" + strings.Repeat("x", 10*1024-1)
	lastChunk := strings.Repeat("x", 10*1024-1) + "L"
	if err := a.write(firstChunk); err != nil {
		t.Fatal(err)
	}
	for range 28 {
		if err := a.write(strings.Repeat("x", 10*1024)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.write(lastChunk); err != nil {
		t.Fatal(err)
	}
	got := a.finalize(5000)
	if !strings.Contains(got, "only tail kept") {
		t.Errorf("over limit should contain banner: %q...", firstN(got, 50))
	}
	if !strings.Contains(got, "L") {
		t.Error("over limit should keep the tail marker L")
	}
	if strings.Contains(got, "F") {
		t.Error("over limit should not keep the head marker F (middle should be dropped)")
	}
}

// §P1-D empty/single chunk: write("") does not grow used; a single chunk over keep with len==1 is not evicted (guards against empty sliding window).
func TestOutputAccum_SingleChunkNotTrimmed(t *testing.T) {
	a := newOutputAccum(100, 0, "", "") // keep much smaller than the chunk
	if err := a.write(""); err != nil {
		t.Fatal(err)
	}
	if a.used != 0 {
		t.Errorf("after write(\"\") used=%d, want 0", a.used)
	}
	big := strings.Repeat("y", 500)
	if err := a.write(big); err != nil {
		t.Fatal(err)
	}
	if a.cut {
		t.Error("single chunk over keep with len==1 should not set cut (guards against empty sliding window)")
	}
	got := a.finalize(5000)
	if got != big {
		t.Errorf("single chunk should be kept in full (no banner): len got=%d want=%d", len(got), len(big))
	}
}

// §P1-D finalize maxChars fallback: when the sliding window join exceeds maxChars, truncateTail truncates to maxChars runes + prepends a marker.
func TestOutputAccum_FinalizeMaxChars(t *testing.T) {
	a := newOutputAccum(100*1024, 0, "", "")
	a.write(strings.Repeat("z", 8000)) // single chunk 8000 runes < keep, no cut
	got := a.finalize(1000)
	if !strings.HasPrefix(got, "…[output truncated]") {
		t.Errorf("over maxChars should prepend a truncate marker: %q...", firstN(got, 30))
	}
	// marker + 1000 runes.
	want := "…[output truncated]" + strings.Repeat("z", 1000)
	if got != want {
		t.Errorf("finalize maxChars wrong length: got len=%d want len=%d", len(got), len(want))
	}
}

// §P1-D spill off (headSpillBytes=0): file always empty, no disk IO.
func TestOutputAccum_SpillOff(t *testing.T) {
	dir := t.TempDir()
	a := newOutputAccum(100*1024, 0, dir, "prefix")
	for range 30 {
		a.write(strings.Repeat("x", 10*1024))
	}
	_ = a.closeSink()
	if a.file != "" {
		t.Errorf("spill off should leave file empty: %q", a.file)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("spill off should not produce files: %d", len(entries))
	}
}

// §P1-D spill on (headSpillBytes>0): after total crosses the threshold, file is non-empty and contains all chunks.
func TestOutputAccum_SpillOn(t *testing.T) {
	dir := t.TempDir()
	a := newOutputAccum(100*1024, 50*1024, dir, "spill")
	for i := range 30 {
		a.write(strings.Repeat(string(rune('A'+i%26)), 10*1024))
	}
	if err := a.closeSink(); err != nil {
		t.Fatalf("closeSink: %v", err)
	}
	if a.file == "" {
		t.Fatal("spill on should leave file non-empty")
	}
	b, err := os.ReadFile(a.file)
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if len(b) != 30*10*1024 {
		t.Errorf("spilled file should contain all chunks: len=%d, want %d", len(b), 30*10*1024)
	}
	// closeSink is idempotent.
	if err := a.closeSink(); err != nil {
		t.Errorf("idempotent closeSink: %v", err)
	}
}

// §P1-D the spill directory path appears in the finalize banner under "full output: ".
func TestOutputAccum_SpillBannerHasFile(t *testing.T) {
	dir := t.TempDir()
	a := newOutputAccum(100*1024, 50*1024, dir, "spill")
	for range 30 {
		a.write(strings.Repeat("x", 10*1024))
	}
	_ = a.closeSink()
	got := a.finalize(5000)
	if !strings.Contains(got, "full output: ") || !strings.Contains(got, a.file) {
		t.Errorf("spill on banner should contain full output path: %q...", firstN(got, 60))
	}
	_ = filepath.Base(a.file) // keep filepath used
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
