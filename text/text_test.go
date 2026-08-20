package text

import (
	"strings"
	"testing"
)

// Truncate: n<=0 returns input as-is; length<=n not truncated; over n takes first n runes + marker.
func TestTruncate(t *testing.T) {
	cases := []struct {
		s      string
		n      int
		marker string
		want   string
	}{
		{"hello", 0, "…", "hello"},  // n<=0 returns as-is
		{"hello", -1, "…", "hello"}, // n<0 returns as-is
		{"hello", 5, "…", "hello"},  // exactly equal, not truncated
		{"hello", 10, "…", "hello"}, // exceeds length, not truncated
		{"hello", 3, "…", "hel…"},   // truncated
		{"你好世界", 2, "…", "你好…"},     // rune boundary (not bytes)
		{"", 3, "…", ""},            // empty string
	}
	for i, c := range cases {
		if got := Truncate(c.s, c.n, c.marker); got != c.want {
			t.Errorf("case %d: Truncate(%q,%d) = %q, want %q", i, c.s, c.n, got, c.want)
		}
	}
}

// TruncateTail: keeps last n runes, prefixing a truncate marker.
func TestTruncateTail(t *testing.T) {
	if got := TruncateTail("hello", 3, "…"); got != "…llo" {
		t.Errorf("TruncateTail = %q, want …llo", got)
	}
	if got := TruncateTail("你好世界", 2, "…"); got != "…世界" {
		t.Errorf("TruncateTail rune = %q, want …世界", got)
	}
	for _, s := range []string{"hello", "你好"} {
		if got := TruncateTail(s, 10, "…"); got != s { // length<=n not truncated
			t.Errorf("TruncateTail(%q,10) = %q, want as-is", s, got)
		}
	}
	if got := TruncateTail("hello", 0, "…"); got != "hello" { // n<=0 returns as-is
		t.Errorf("TruncateTail n<=0 = %q, want as-is", got)
	}
}

// TruncateHeadTail: keeps head n/4 + tail 3n/4, inserts a marker in the middle; length<=n not truncated; no truncation when head+tail already cover everything.
func TestTruncateHeadTail(t *testing.T) {
	s := "0123456789abcdef" // 16 rune
	got := TruncateHeadTail(s, 8, "…")
	// headN=max(8/4,1)=2, tailN=max(8-2,1)=6 → "01" + "…" + "abcdef"
	if got != "01…abcdef" {
		t.Errorf("TruncateHeadTail(16,8) = %q, want 01…abcdef", got)
	}
	// rune boundary
	got = TruncateHeadTail("你好世界你好世界你好世界", 4, "…") // 12 runes
	if !strings.HasPrefix(got, "你") || !strings.HasSuffix(got, "界") || !strings.Contains(got, "…") {
		t.Errorf("TruncateHeadTail rune = %q, want head+marker+tail", got)
	}
	if got := TruncateHeadTail("short", 100, "…"); got != "short" { // length<=n not truncated
		t.Errorf("TruncateHeadTail short = %q, want as-is", got)
	}
	if got := TruncateHeadTail("short", 0, "…"); got != "short" { // n<=0 returns as-is
		t.Errorf("TruncateHeadTail n<=0 = %q, want as-is", got)
	}
	// when head+tail window already covers everything, not truncated (no marker noise): n large enough that headN+tailN>=len
	if got := TruncateHeadTail("abc", 3, "…"); got != "abc" {
		t.Errorf("TruncateHeadTail full-cover = %q, want abc (no marker added)", got)
	}
}

// CountCharsLocal: counts CJK (Han/Hiragana/Katakana/Hangul) and non-CJK separately.
func TestCountCharsLocal(t *testing.T) {
	cases := []struct {
		s      string
		nonCJK int
		cjk    int
	}{
		{"abc", 3, 0},
		{"你好", 0, 2},  // Han
		{"안녕", 0, 2},  // Hangul
		{"a中b", 2, 1}, // mixed
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

// NowMs returns a positive Unix millisecond timestamp.
func TestNowMs(t *testing.T) {
	ms := NowMs()
	if ms <= 0 {
		t.Errorf("NowMs = %d, want > 0", ms)
	}
	// monotonically non-decreasing (a later consecutive call is not less than the earlier one)
	if ms2 := NowMs(); ms2 < ms {
		t.Errorf("NowMs not monotonic: %d -> %d", ms, ms2)
	}
}
