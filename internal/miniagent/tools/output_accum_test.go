package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §P1-D under-limit：写少量（< keep）→ finalize 无 banner、拼接等价原文。
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

// §P1-D over-limit-keeps-tail：超 keep 丢中段保尾，finalize 含 banner、含尾部、不含首部。
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
	if !strings.Contains(got, "仅保留尾部") {
		t.Errorf("超限应含 banner：%q...", firstN(got, 50))
	}
	if !strings.Contains(got, "L") {
		t.Error("超限应保留尾部标记 L")
	}
	if strings.Contains(got, "F") {
		t.Error("超限不应保留首部标记 F（中段应被丢）")
	}
}

// §P1-D empty/单 chunk：write("") 不增长 used；单 chunk 超 keep 且 len==1 不剔除（防空滑窗）。
func TestOutputAccum_SingleChunkNotTrimmed(t *testing.T) {
	a := newOutputAccum(100, 0, "", "") // keep 远小于 chunk
	if err := a.write(""); err != nil {
		t.Fatal(err)
	}
	if a.used != 0 {
		t.Errorf("write(\"\") 后 used=%d, want 0", a.used)
	}
	big := strings.Repeat("y", 500)
	if err := a.write(big); err != nil {
		t.Fatal(err)
	}
	if a.cut {
		t.Error("单 chunk 超 keep 且 len==1 不应置 cut（防空滑窗）")
	}
	got := a.finalize(5000)
	if got != big {
		t.Errorf("单 chunk 应完整保留（无 banner）: len got=%d want=%d", len(got), len(big))
	}
}

// §P1-D finalize maxChars 兜底：滑窗 join 超 maxChars 时 truncateTail 截到 maxChars rune + 前置 marker。
func TestOutputAccum_FinalizeMaxChars(t *testing.T) {
	a := newOutputAccum(100*1024, 0, "", "")
	a.write(strings.Repeat("z", 8000)) // 单 chunk 8000 rune < keep，不 cut
	got := a.finalize(1000)
	if !strings.HasPrefix(got, "…[输出已截断]") {
		t.Errorf("超 maxChars 应前置截断 marker：%q...", firstN(got, 30))
	}
	// marker + 1000 rune。
	want := "…[输出已截断]" + strings.Repeat("z", 1000)
	if got != want {
		t.Errorf("finalize maxChars 长度错: got len=%d want len=%d", len(got), len(want))
	}
}

// §P1-D spill off（headSpillBytes=0）：file 始终空、无落盘 IO。
func TestOutputAccum_SpillOff(t *testing.T) {
	dir := t.TempDir()
	a := newOutputAccum(100*1024, 0, dir, "prefix")
	for range 30 {
		a.write(strings.Repeat("x", 10*1024))
	}
	_ = a.closeSink()
	if a.file != "" {
		t.Errorf("spill off 时 file 应为空: %q", a.file)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("spill off 不应产生文件: %d", len(entries))
	}
}

// §P1-D spill on（headSpillBytes>0）：total 过阈值后 file 非空、文件内容含全部 chunk。
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
		t.Fatal("spill on 时 file 应非空")
	}
	b, err := os.ReadFile(a.file)
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if len(b) != 30*10*1024 {
		t.Errorf("落盘文件应含全部 chunk: len=%d, want %d", len(b), 30*10*1024)
	}
	// closeSink 幂等。
	if err := a.closeSink(); err != nil {
		t.Errorf("幂等 closeSink: %v", err)
	}
}

// §P1-D spill 落盘目录路径出现在 finalize banner 的「全文：」中。
func TestOutputAccum_SpillBannerHasFile(t *testing.T) {
	dir := t.TempDir()
	a := newOutputAccum(100*1024, 50*1024, dir, "spill")
	for range 30 {
		a.write(strings.Repeat("x", 10*1024))
	}
	_ = a.closeSink()
	got := a.finalize(5000)
	if !strings.Contains(got, "全文：") || !strings.Contains(got, a.file) {
		t.Errorf("spill on 的 banner 应含全文路径：%q...", firstN(got, 60))
	}
	_ = filepath.Base(a.file) // keep filepath used
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
