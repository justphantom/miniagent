// Package text provides plain-text helpers shared by the core and addon subpackages
// (compaction/event/builtin/provider/openai): rune truncation (head / tail / head+tail split),
// CJK/non-CJK rune counting, and Unix millisecond timestamps.
// All are pure functions independent of agent domain types, so they form a repo-level shared layer
// (internal/text), eliminating divergent truncation/counting implementations across packages.
package text

import (
	"time"
	"unicode"
)

// NowMs returns a Unix millisecond timestamp for stamping messages (e.g. the compaction summaryMsg.Ts).
func NowMs() int64 {
	return time.Now().UnixMilli()
}

// CountCharsLocal counts the non-CJK / CJK runes in s (same basis as token estimation).
func CountCharsLocal(s string) (nonCJK, cjk int) {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			nonCJK++
		}
	}
	return nonCJK, cjk
}

// Truncate clamps s to n runes and appends marker when it truncated. n<=0 returns s as-is.
func Truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}

// TruncateTail keeps the last n runes of s, prefixing marker when it truncates.
// Serves the tail fallback (after the shell accumulator keeps the tail and drops the middle, the window tail is truncated to maxChars). n<=0 returns s as-is; len<=n is not truncated.
func TruncateTail(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return marker + string(r[len(r)-n:])
}

// TruncateHeadTail truncates s to roughly n runes, keeping a head of headN + a tail of tailN joined by marker.
// Used for tool results whose key information is in the tail: head-only would drop compile/test error
// summaries, limit-hit notices, and other diagnostics.
// Head gets n/4 (leading context / command echo), tail gets 3n/4 (where error conclusions cluster). n<=0 returns s as-is; len<=n is not truncated.
// marker is placed at the middle omission (different semantics from Truncate's trailing marker).
func TruncateHeadTail(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	headN := max(n/4, 1)
	tailN := max(n-headN, 1)
	if headN+tailN >= len(r) {
		return s // head+tail window already covers everything, no truncation needed (a marker would only add noise)
	}
	return string(r[:headN]) + marker + string(r[len(r)-tailN:])
}
