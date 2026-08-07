package text

import (
	"strings"
	"testing"
)

// Truncate：n<=0 原样返回；长度<=n 不截；超 n 取前 n rune + marker。
func TestTruncate(t *testing.T) {
	cases := []struct {
		s      string
		n      int
		marker string
		want   string
	}{
		{"hello", 0, "…", "hello"},  // n<=0 原样
		{"hello", -1, "…", "hello"}, // n<0 原样
		{"hello", 5, "…", "hello"},  // 恰好等于，不截
		{"hello", 10, "…", "hello"}, // 超出长度，不截
		{"hello", 3, "…", "hel…"},   // 截断
		{"你好世界", 2, "…", "你好…"},     // rune 边界（非字节）
		{"", 3, "…", ""},            // 空串
	}
	for i, c := range cases {
		if got := Truncate(c.s, c.n, c.marker); got != c.want {
			t.Errorf("case %d: Truncate(%q,%d) = %q, want %q", i, c.s, c.n, got, c.want)
		}
	}
}

// TruncateTail：保末尾 n rune，截断前置 marker。
func TestTruncateTail(t *testing.T) {
	if got := TruncateTail("hello", 3, "…"); got != "…llo" {
		t.Errorf("TruncateTail = %q, want …llo", got)
	}
	if got := TruncateTail("你好世界", 2, "…"); got != "…世界" {
		t.Errorf("TruncateTail rune = %q, want …世界", got)
	}
	for _, s := range []string{"hello", "你好"} {
		if got := TruncateTail(s, 10, "…"); got != s { // 长度<=n 不截
			t.Errorf("TruncateTail(%q,10) = %q, want 原样", s, got)
		}
	}
	if got := TruncateTail("hello", 0, "…"); got != "hello" { // n<=0 原样
		t.Errorf("TruncateTail n<=0 = %q, want 原样", got)
	}
}

// TruncateHeadTail：保头 n/4 + 尾 3n/4，中段插 marker；长度<=n 不截；头尾已覆盖全部时不截。
func TestTruncateHeadTail(t *testing.T) {
	s := "0123456789abcdef" // 16 rune
	got := TruncateHeadTail(s, 8, "…")
	// headN=max(8/4,1)=2, tailN=max(8-2,1)=6 → "01" + "…" + "abcdef"
	if got != "01…abcdef" {
		t.Errorf("TruncateHeadTail(16,8) = %q, want 01…abcdef", got)
	}
	// rune 边界
	got = TruncateHeadTail("你好世界你好世界你好世界", 4, "…") // 12 rune
	if !strings.HasPrefix(got, "你") || !strings.HasSuffix(got, "界") || !strings.Contains(got, "…") {
		t.Errorf("TruncateHeadTail rune = %q, want 头+marker+尾", got)
	}
	if got := TruncateHeadTail("short", 100, "…"); got != "short" { // 长度<=n 不截
		t.Errorf("TruncateHeadTail short = %q, want 原样", got)
	}
	if got := TruncateHeadTail("short", 0, "…"); got != "short" { // n<=0 原样
		t.Errorf("TruncateHeadTail n<=0 = %q, want 原样", got)
	}
	// 头尾窗口已覆盖全部时不截（不增 marker 噪音）：n 大到 headN+tailN>=len
	if got := TruncateHeadTail("abc", 3, "…"); got != "abc" {
		t.Errorf("TruncateHeadTail full-cover = %q, want abc（不加 marker）", got)
	}
}

// CountCharsLocal：CJK（Han/Hiragana/Katakana/Hangul）与非 CJK 分流计数。
func TestCountCharsLocal(t *testing.T) {
	cases := []struct {
		s      string
		nonCJK int
		cjk    int
	}{
		{"abc", 3, 0},
		{"你好", 0, 2},  // Han
		{"안녕", 0, 2},  // Hangul
		{"a中b", 2, 1}, // 混合
		{"", 0, 0},
		{"ひらがな", 0, 4}, // Hiragana
		{"カタカナ", 0, 4}, // Katakana
	}
	for i, c := range cases {
		nonCJK, cjk := CountCharsLocal(c.s)
		if nonCJK != c.nonCJK || cjk != c.cjk {
			t.Errorf("case %d: CountCharsLocal(%q) = (%d,%d), want (%d,%d)", i, c.s, nonCJK, cjk, c.nonCJK, c.cjk)
		}
	}
}

// NowMs 返回正的 Unix 毫秒戳。
func TestNowMs(t *testing.T) {
	ms := NowMs()
	if ms <= 0 {
		t.Errorf("NowMs = %d, want > 0", ms)
	}
	// 单调递增（连续调用后者不小于前者）
	if ms2 := NowMs(); ms2 < ms {
		t.Errorf("NowMs 非单调: %d -> %d", ms, ms2)
	}
}
